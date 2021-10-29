package oracle

import (
	"bytes"
	"database/sql"
	"fmt"
	"reflect"

	"github.com/thoas/go-funk"
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"

	
	gormSchema "gorm.io/gorm/schema"

	"github.com/ddelpero/oracle/clauses"
)

func Create(db *gorm.DB) {
	stmt := db.Statement
	schema := stmt.Schema
	boundVars := make(map[string]int)

	if stmt == nil || schema == nil {
		return
	}

	hasDefaultValues := len(schema.FieldsWithDefaultDBValue) > 0

	if !stmt.Unscoped {
		for _, c := range schema.CreateClauses {
			stmt.AddClause(c)
		}
	}

	if stmt.SQL.String() == "" {
		values := ConvertToCreateValues(stmt)
		onConflict, hasConflict := stmt.Clauses["ON CONFLICT"].Expression.(clause.OnConflict)
		// are all columns in value the primary fields in schema only?
		if hasConflict && funk.Contains(
			funk.Map(values.Columns, func(c clause.Column) string { return c.Name }),
			funk.Map(schema.PrimaryFields, func(field *gormSchema.Field) string { return field.DBName }),
		) {
			stmt.AddClauseIfNotExists(clauses.Merge{
				Using: []clause.Interface{
					clause.Select{
						Columns: funk.Map(values.Columns, func(column clause.Column) clause.Column {
							// HACK: I can not come up with a better alternative for now
							// I want to add a value to the list of variable and then capture the bind variable position as well
							buf := bytes.NewBufferString("")
							stmt.Vars = append(stmt.Vars, values.Values[0][funk.IndexOf(values.Columns, column)])
							stmt.BindVarTo(buf, stmt, nil)

							column.Alias = column.Name
							// then the captured bind var will be the name
							column.Name = buf.String()
							return column
						}).([]clause.Column),
					},
					clause.From{
						Tables: []clause.Table{{Name: db.Dialector.(Dialector).DummyTableName()}},
					},
				},
				On: funk.Map(schema.PrimaryFields, func(field *gormSchema.Field) clause.Expression {
					return clause.Eq{
						Column: clause.Column{Table: stmt.Table, Name: field.DBName},
						Value:  clause.Column{Table: clauses.MergeDefaultExcludeName(), Name: field.DBName},
					}
				}).([]clause.Expression),
			})
			stmt.AddClauseIfNotExists(clauses.WhenMatched{Set: onConflict.DoUpdates})
			stmt.AddClauseIfNotExists(clauses.WhenNotMatched{Values: values})

			stmt.Build("MERGE", "WHEN MATCHED", "WHEN NOT MATCHED")
		} else {
			stmt.AddClauseIfNotExists(clause.Insert{Table: clause.Table{Name: stmt.Table}})
			stmt.AddClause(clause.Values{Columns: values.Columns, Values: [][]interface{}{values.Values[0]}})
			if hasDefaultValues {
				stmt.AddClauseIfNotExists(clause.Returning{
					Columns: funk.Map(schema.FieldsWithDefaultDBValue, func(field *gormSchema.Field) clause.Column {
						return clause.Column{Name: field.DBName}
					}).([]clause.Column),
				})
			}
			stmt.Build("INSERT", "VALUES", "RETURNING")
			if hasDefaultValues {
				stmt.WriteString(" INTO ")
				for idx, field := range schema.FieldsWithDefaultDBValue {
					if idx > 0 {
						stmt.WriteByte(',')
					}
					boundVars[field.Name] = len(stmt.Vars)
					stmt.AddVar(stmt, sql.Out{Dest: reflect.New(field.FieldType).Interface()})
				}
			}
		}

		if !db.DryRun {
			for idx, vals := range values.Values {
				// HACK HACK: replace values one by one, assuming its value layout will be the same all the time, i.e. aligned
				for idx, val := range vals {
					switch v := val.(type) {
					case bool:
						if v {
							val = 1
						} else {
							val = 0
						}
					}

					//don't add the returning clause to the bind vars
					_, isClauseExpr := val.(clause.Expr)
					if !isClauseExpr {
						stmt.Vars[idx] = val
					}
				}
				// and then we insert each row one by one then put the returning values back (i.e. last return id => smart insert)
				// we keep track of the index so that the sub-reflected value is also correct

				// BIG BUG: what if any of the transactions failed? some result might already be inserted that oracle is so
				// sneaky that some transaction inserts will exceed the buffer and so will be pushed at unknown point,
				// resulting in dangling row entries, so we might need to delete them if an error happens

				switch result, err := stmt.ConnPool.ExecContext(stmt.Context, stmt.SQL.String(), stmt.Vars...); err {
				case nil: // success
					db.RowsAffected, _ = result.RowsAffected()

					insertTo := stmt.ReflectValue
					switch insertTo.Kind() {
					case reflect.Slice, reflect.Array:
						insertTo = insertTo.Index(idx)
					}

					if hasDefaultValues {
						// bind returning value back to reflected value in the respective fields
						funk.ForEach(
							funk.Filter(schema.FieldsWithDefaultDBValue, func(field *gormSchema.Field) bool {
								return funk.Contains(boundVars, field.Name)
							}),
							func(field *gormSchema.Field) {
								switch insertTo.Kind() {
								case reflect.Struct:
									if err = field.Set(insertTo, stmt.Vars[boundVars[field.Name]].(sql.Out).Dest); err != nil {
										db.AddError(err)
									}
								case reflect.Map:
									// todo 设置id的值
								}
							},
						)
					}
				default: // failure
					db.AddError(err)
				}
			}
		}
	}
}

// ConvertToCreateValues convert to create values
// Copy of func from gorm callbacks/create.go with change to struct FieldsWithDefaultDBValue
func ConvertToCreateValues(stmt *gorm.Statement) (values clause.Values) {
	curTime := stmt.DB.NowFunc()

	switch value := stmt.Dest.(type) {
	case map[string]interface{}:
		values = callbacks.ConvertMapToValuesForCreate(stmt, value)
	case *map[string]interface{}:
		values = callbacks.ConvertMapToValuesForCreate(stmt, *value)
	case []map[string]interface{}:
		values = callbacks.ConvertSliceOfMapToValuesForCreate(stmt, value)
	case *[]map[string]interface{}:
		values = callbacks.ConvertSliceOfMapToValuesForCreate(stmt, *value)
	default:
		var (
			selectColumns, restricted = stmt.SelectAndOmitColumns(true, false)
			_, updateTrackTime        = stmt.Get("gorm:update_track_time")
			isZero                    bool
		)
		stmt.Settings.Delete("gorm:update_track_time")

		values = clause.Values{Columns: make([]clause.Column, 0, len(stmt.Schema.DBNames))}

		for _, db := range stmt.Schema.DBNames {
			if field := stmt.Schema.FieldsByDBName[db]; !field.HasDefaultValue || field.DefaultValueInterface != nil {
				if v, ok := selectColumns[db]; (ok && v) || (!ok && (!restricted || field.AutoCreateTime > 0 || field.AutoUpdateTime > 0)) {
					values.Columns = append(values.Columns, clause.Column{Name: db})
				}
			}
		}

		switch stmt.ReflectValue.Kind() {
		case reflect.Slice, reflect.Array:
			stmt.SQL.Grow(stmt.ReflectValue.Len() * 18)
			values.Values = make([][]interface{}, stmt.ReflectValue.Len())
			defaultValueFieldsHavingValue := map[*gormSchema.Field][]interface{}{}
			if stmt.ReflectValue.Len() == 0 {
				stmt.AddError(gorm.ErrEmptySlice)
				return
			}

			for i := 0; i < stmt.ReflectValue.Len(); i++ {
				rv := reflect.Indirect(stmt.ReflectValue.Index(i))
				if !rv.IsValid() {
					stmt.AddError(fmt.Errorf("slice data #%v is invalid: %w", i, gorm.ErrInvalidData))
					return
				}

				values.Values[i] = make([]interface{}, len(values.Columns))
				for idx, column := range values.Columns {
					field := stmt.Schema.FieldsByDBName[column.Name]
					if values.Values[i][idx], isZero = field.ValueOf(rv); isZero {
						if field.DefaultValueInterface != nil {
							values.Values[i][idx] = field.DefaultValueInterface
							field.Set(rv, field.DefaultValueInterface)
						} else if field.AutoCreateTime > 0 || field.AutoUpdateTime > 0 {
							field.Set(rv, curTime)
							values.Values[i][idx], _ = field.ValueOf(rv)
						}
					} else if field.AutoUpdateTime > 0 && updateTrackTime {
						field.Set(rv, curTime)
						values.Values[i][idx], _ = field.ValueOf(rv)
					}
				}

				for _, field := range stmt.Schema.FieldsWithDefaultDBValue {
					if v, ok := selectColumns[field.DBName]; (ok && v) || (!ok && !restricted) {
						if v, isZero := field.ValueOf(rv); !isZero {
							if len(defaultValueFieldsHavingValue[field]) == 0 {
								defaultValueFieldsHavingValue[field] = make([]interface{}, stmt.ReflectValue.Len())
							}
							defaultValueFieldsHavingValue[field][i] = v
						}
					}
				}
			}

			for field, vs := range defaultValueFieldsHavingValue {
				values.Columns = append(values.Columns, clause.Column{Name: field.DBName})
				for idx := range values.Values {
					if vs[idx] == nil {
						values.Values[idx] = append(values.Values[idx], stmt.Dialector.DefaultValueOf(field))
					} else {
						values.Values[idx] = append(values.Values[idx], vs[idx])
					}
				}
			}
		case reflect.Struct:
			values.Values = [][]interface{}{make([]interface{}, len(values.Columns))}
			for idx, column := range values.Columns {
				field := stmt.Schema.FieldsByDBName[column.Name]
				if values.Values[0][idx], isZero = field.ValueOf(stmt.ReflectValue); isZero {
					if field.DefaultValueInterface != nil {
						values.Values[0][idx] = field.DefaultValueInterface
						field.Set(stmt.ReflectValue, field.DefaultValueInterface)
					} else if field.AutoCreateTime > 0 || field.AutoUpdateTime > 0 {
						field.Set(stmt.ReflectValue, curTime)
						values.Values[0][idx], _ = field.ValueOf(stmt.ReflectValue)
					}
				} else if field.AutoUpdateTime > 0 && updateTrackTime {
					field.Set(stmt.ReflectValue, curTime)
					values.Values[0][idx], _ = field.ValueOf(stmt.ReflectValue)
				}
			}

			for _, field := range stmt.Schema.FieldsWithDefaultDBValue {
				if v, ok := selectColumns[field.DBName]; (ok && v) || (!ok && !restricted) {
					_, isZero := field.ValueOf(stmt.ReflectValue)
					if !isZero {

						values.Columns = append(values.Columns, clause.Column{Name: field.DBName})
						// values.Values[0] = append(values.Values[0], v)
						// Setting this to v sets the values clause to the value passed in the struct,
						// which since it isZero will be 0
						// Instead, for Oracle at least, it should be the field.DefaultValue
						values.Values[0] = append(values.Values[0], clause.Expr{SQL: field.DefaultValue})
					}
				}
			}
		default:
			stmt.AddError(gorm.ErrInvalidData)
		}
	}

	if c, ok := stmt.Clauses["ON CONFLICT"]; ok {
		if onConflict, _ := c.Expression.(clause.OnConflict); onConflict.UpdateAll {
			if stmt.Schema != nil && len(values.Columns) >= 1 {
				selectColumns, restricted := stmt.SelectAndOmitColumns(true, true)

				columns := make([]string, 0, len(values.Columns)-1)
				for _, column := range values.Columns {
					if field := stmt.Schema.LookUpField(column.Name); field != nil {
						if v, ok := selectColumns[field.DBName]; (ok && v) || (!ok && !restricted) {
							if !field.PrimaryKey && (!field.HasDefaultValue || field.DefaultValueInterface != nil) && field.AutoCreateTime == 0 {
								if field.AutoUpdateTime > 0 {
									assignment := clause.Assignment{Column: clause.Column{Name: field.DBName}, Value: curTime}
									switch field.AutoUpdateTime {
									case gormSchema.UnixNanosecond:
										assignment.Value = curTime.UnixNano()
									case gormSchema.UnixMillisecond:
										assignment.Value = curTime.UnixNano() / 1e6
									case gormSchema.UnixSecond:
										assignment.Value = curTime.Unix()
									}

									onConflict.DoUpdates = append(onConflict.DoUpdates, assignment)
								} else {
									columns = append(columns, column.Name)
								}
							}
						}
					}
				}

				onConflict.DoUpdates = append(onConflict.DoUpdates, clause.AssignmentColumns(columns)...)

				// use primary fields as default OnConflict columns
				if len(onConflict.Columns) == 0 {
					for _, field := range stmt.Schema.PrimaryFields {
						onConflict.Columns = append(onConflict.Columns, clause.Column{Name: field.DBName})
					}
				}
				stmt.AddClause(onConflict)
			}
		}
	}

	return values
}
