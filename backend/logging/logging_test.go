package logging

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestDebugfRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	SetDebug(false)
	Debugf("hidden line plugin_id=%s", "example")
	if buf.Len() != 0 {
		t.Fatalf("debug line was written with debug disabled: %q", buf.String())
	}

	SetDebug(true)
	Debugf("visible line plugin_id=%s", "example")
	if got := buf.String(); !strings.Contains(got, "debug visible line plugin_id=example") {
		t.Fatalf("debug line missing with debug enabled: %q", got)
	}
	SetDebug(false)
}
