package store

import (
	"reflect"
	"testing"
)

// TestToInt64 covers the tolerant coercion of a Redis/Lua reply element to
// int64. This is load-bearing: it parses the {status, applied, newhp} the damage
// script returns, so a wrong coercion would corrupt HP or the applied amount.
// go-redis may deliver integers as int64 (common) but the helper also tolerates
// int and float64; anything unexpected must be 0, never a panic.
func TestToInt64(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int64
	}{
		{"int64", int64(42), 42},
		{"negative int64", int64(-1), -1},
		{"int", int(7), 7},
		{"float64 whole", float64(100), 100},
		{"float64 truncates toward zero", float64(3.9), 3},
		{"string is not coerced", "5", 0},
		{"nil defaults to zero", nil, 0},
		{"bool defaults to zero", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := toInt64(c.in); got != c.want {
				t.Errorf("toInt64(%#v) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// TestDecodePayload covers turning a stored reward JSONB blob back into the map
// returned in the claim response. Empty/nil input and malformed JSON must yield
// nil (a safe, omittable response field) rather than panic.
func TestDecodePayload(t *testing.T) {
	t.Run("valid object", func(t *testing.T) {
		got := decodePayload([]byte(`{"gold":100,"items":["Common Chest"]}`))
		want := map[string]any{
			"gold":  float64(100), // JSON numbers decode to float64
			"items": []any{"Common Chest"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("decodePayload = %#v, want %#v", got, want)
		}
	})
	t.Run("nil input", func(t *testing.T) {
		if got := decodePayload(nil); got != nil {
			t.Errorf("decodePayload(nil) = %#v, want nil", got)
		}
	})
	t.Run("empty input", func(t *testing.T) {
		if got := decodePayload([]byte{}); got != nil {
			t.Errorf("decodePayload(empty) = %#v, want nil", got)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		if got := decodePayload([]byte(`{not json`)); got != nil {
			t.Errorf("decodePayload(malformed) = %#v, want nil", got)
		}
	})
	t.Run("non-object JSON", func(t *testing.T) {
		// A JSON array cannot unmarshal into map[string]any => nil.
		if got := decodePayload([]byte(`[1,2,3]`)); got != nil {
			t.Errorf("decodePayload(array) = %#v, want nil", got)
		}
	})
}
