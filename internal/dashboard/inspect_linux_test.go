//go:build linux

package dashboard

import (
	"strings"
	"testing"
)

func TestInspectorKeys(t *testing.T) {
	var decoder keyDecoder
	keys := decoder.Feed([]byte("tnp \t"))
	if got := strings.Join(keys, ","); got != "tables,next-router,previous-router,pause,tab" {
		t.Fatalf("inspector navigation: %s", got)
	}
}
