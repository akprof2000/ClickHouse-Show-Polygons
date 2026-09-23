package main

// Превращение строк native-протокола в значения, пригодные для JSON и для
// остального кода. Раньше эту работу делал сам ClickHouse форматом JSON;
// теперь драйвер отдаёт типизированные значения Go, и приводить их к общему
// виду приходится здесь.

import (
	"reflect"
	"strconv"
	"time"
)

// reflectNew создаёт приёмник для колонки по её типу. Запросы у нас
// произвольные, поэтому структуру строки заранее мы не знаем.
func reflectNew(t reflect.Type) any {
	if t == nil {
		return new(any)
	}
	return reflect.New(t).Interface()
}

// derefAny разыменовывает указатель, полученный от reflectNew.
func derefAny(v any) any {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return nil
	}
	return rv.Interface()
}

// jsonValue приводит значение к тому виду, в котором его ждут и браузер, и
// разбор геометрии.
//
// Две вещи здесь не случайны:
//   - 64-битные целые отдаём строками. Так же поступает ClickHouse в формате
//     JSON, и не зря: в JavaScript число больше 2^53 теряет точность, а у нас
//     это идентификатор объекта (cityHash64);
//   - геометрию раскладываем в массивы [x, y]. Драйвер отдаёт типы orb
//     (MultiPolygon — это [][][]Point, Point — [2]float64), и обход по
//     массивам превращает их ровно в те вложенные списки, которые ожидает
//     GeoJSON и функция geomBBox.
func jsonValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string, bool, float32, float64:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case []byte:
		return string(x)
	case time.Time:
		return x.Format(time.RFC3339)
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return nil
		}
		return jsonValue(rv.Elem().Interface())
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = jsonValue(rv.Index(i).Interface())
		}
		return out
	case reflect.Map:
		out := make(map[string]any, rv.Len())
		for _, k := range rv.MapKeys() {
			out[toString(k.Interface())] = jsonValue(rv.MapIndex(k).Interface())
		}
		return out
	case reflect.Struct:
		// Decimal, UUID, IP и подобные умеют показывать себя строкой
		if s, ok := v.(interface{ String() string }); ok {
			return s.String()
		}
	}
	return v
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if s, ok := v.(interface{ String() string }); ok {
		return s.String()
	}
	return toStringFallback(v)
}

func toStringFallback(v any) string {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(rv.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(rv.Float(), 'g', -1, 64)
	}
	return ""
}
