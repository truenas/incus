package filter

import (
	"reflect"
	"strings"
)

// DotPrefixMatch finds the shortest unambiguous identifier for a given namespace.
func DotPrefixMatch(short string, full string) bool {
	fullMembs := strings.Split(full, ".")
	shortMembs := strings.Split(short, ".")

	if len(fullMembs) != len(shortMembs) {
		return false
	}

	for i := range fullMembs {
		if !strings.HasPrefix(fullMembs[i], shortMembs[i]) {
			return false
		}
	}

	return true
}

// ValueOf returns the value of the given field.
func ValueOf(obj any, field string) any {
	value := reflect.ValueOf(obj)
	typ := value.Type()
	parts := strings.Split(field, ".")

	key := parts[0]
	rest := strings.Join(parts[1:], ".")

	if value.Kind() == reflect.Map {
		switch reflect.TypeOf(obj).Elem().Kind() {
		case reflect.String:
			m := value.Interface().(map[string]string)
			for k, v := range m {
				if DotPrefixMatch(field, k) {
					return v
				}
			}

			return m[field]

		case reflect.Map:
			for _, entry := range value.MapKeys() {
				if entry.Interface() != key {
					continue
				}

				m := value.MapIndex(entry)
				return ValueOf(m.Interface(), rest)
			}

			return nil

		default:
			return nil
		}
	}

	for i := 0; i < value.NumField(); i++ {
		fieldValue := value.Field(i)
		fieldType := typ.Field(i)
		yaml := fieldType.Tag.Get("yaml")

		if yaml == ",inline" {
			v := ValueOf(fieldValue.Interface(), field)
			if v != nil {
				return v
			}
		}

		yamlKey, _, _ := strings.Cut(yaml, ",")
		if yamlKey == key {
			v := fieldValue.Interface()
			if len(parts) == 1 {
				return v
			}

			return ValueOf(v, rest)
		}
	}

	return nil
}
