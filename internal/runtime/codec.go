package runtime

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

// DecodeSnapshot reads a trusted versioned database projection using the wider
// internal decimal envelope. It is not the public plan or amount JSON decoder.
func DecodeSnapshot(raw json.RawMessage) (State, error) {
	var state State
	if err := decodeInternal(raw, &state); err != nil {
		return State{}, err
	}
	if state.Version != StateVersion || state.Profile != ProfilePaper || state.Book == nil {
		return State{}, ErrInvalid
	}
	return state, nil
}

// Trusted persisted state has a wider decimal envelope than public JSON. This
// decoder is local to versioned snapshots; external Decimal decoding stays strict.
func decodeInternal(data []byte, dst any) error {
	if len(data) > 1<<20 {
		return ErrHistoryLimit
	}
	v := reflect.ValueOf(dst)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return ErrInvalid
	}
	return decodeValue(data, v.Elem())
}
func decodeValue(raw json.RawMessage, v reflect.Value) error {
	if v.Type() == reflect.TypeOf(json.RawMessage{}) {
		return json.Unmarshal(raw, v.Addr().Interface())
	}
	if v.Type() == reflect.TypeOf(d.Decimal{}) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		value, err := d.ParseInternalDecimal(s)
		if err != nil {
			return err
		}
		v.Set(reflect.ValueOf(value))
		return nil
	}
	if v.Type() == reflect.TypeOf(time.Time{}) {
		return json.Unmarshal(raw, v.Addr().Interface())
	}
	switch v.Kind() {
	case reflect.Pointer:
		if string(raw) == "null" {
			v.SetZero()
			return nil
		}
		v.Set(reflect.New(v.Type().Elem()))
		return decodeValue(raw, v.Elem())
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if field.PkgPath != "" {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			if data, exists := fields[name]; exists {
				if err := decodeValue(data, v.Field(i)); err != nil {
					return fmt.Errorf("snapshot %s: %w", name, err)
				}
			}
		}
		return nil
	case reflect.Slice:
		if string(raw) == "null" {
			v.SetZero()
			return nil
		}
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		if len(values) > 8192 {
			return ErrHistoryLimit
		}
		v.Set(reflect.MakeSlice(v.Type(), len(values), len(values)))
		for i, data := range values {
			if err := decodeValue(data, v.Index(i)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if string(raw) == "null" {
			v.SetZero()
			return nil
		}
		if v.Type().Key().Kind() != reflect.String {
			return ErrInvalid
		}
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		if len(values) > 8192 {
			return ErrHistoryLimit
		}
		v.Set(reflect.MakeMapWithSize(v.Type(), len(values)))
		for key, data := range values {
			item := reflect.New(v.Type().Elem()).Elem()
			if err := decodeValue(data, item); err != nil {
				return err
			}
			v.SetMapIndex(reflect.ValueOf(key).Convert(v.Type().Key()), item)
		}
		return nil
	default:
		return json.Unmarshal(raw, v.Addr().Interface())
	}
}
func cloneState(s State) (State, error) {
	var out State
	raw, err := json.Marshal(s)
	if err != nil {
		return out, err
	}
	return out, decodeInternal(raw, &out)
}
