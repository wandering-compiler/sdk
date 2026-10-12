package kvfs

import (
	"reflect"
	"testing"
)

// The half a reading tier receives can only read: no method on it stores,
// replaces or deletes an object, so no second component can derive keys.
// A full Driver still narrows to it.
func TestReader_CarriesNoWriteMethod(t *testing.T) {
	readOnly := map[string]bool{"Open": true, "Stat": true, "OpenSeekable": true}
	rt := reflect.TypeOf((*Reader)(nil)).Elem()
	for i := 0; i < rt.NumMethod(); i++ {
		if name := rt.Method(i).Name; !readOnly[name] {
			t.Errorf("Reader carries %s; the reading tiers would be able to call it", name)
		}
	}
	var _ Reader = Driver(nil) // a Driver narrows to its Reader half
}
