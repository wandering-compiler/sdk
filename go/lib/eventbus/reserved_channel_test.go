package eventbus

import (
	"errors"
	"testing"
)

func TestValidateChannel_ReservedW17(t *testing.T) {
	for name, refused := range map[string]bool{"w17": true, "W17_REQUESTS": true, "w17_x": true, "w17x": false, "app_w17": false} {
		err := ValidateChannel(name)
		if (err != nil) != refused || (err != nil && !errors.Is(err, ErrInvalidChannel)) {
			t.Errorf("%s: err = %v, refused want %v", name, err, refused)
		}
	}
}
