package cli

import (
	"testing"

	"github.com/sirupsen/logrus"
)

func TestResolveLimaLogLevel(t *testing.T) {
	cases := []struct {
		flag  string
		debug bool
		want  logrus.Level
	}{
		{"", false, logrus.ErrorLevel}, // default: quiet
		{"", true, logrus.InfoLevel},   // --debug surfaces Lima at info
		{"info", false, logrus.InfoLevel},
		{"info", true, logrus.InfoLevel}, // explicit flag wins over --debug
		{"warn", false, logrus.WarnLevel},
		{"debug", false, logrus.DebugLevel},
		{"off", false, logrus.PanicLevel},
		{"none", false, logrus.PanicLevel},
	}
	for _, c := range cases {
		got, err := resolveLimaLogLevel(c.flag, c.debug)
		if err != nil {
			t.Errorf("resolveLimaLogLevel(%q,%v) unexpected error: %v", c.flag, c.debug, err)
			continue
		}
		if got != c.want {
			t.Errorf("resolveLimaLogLevel(%q,%v) = %v, want %v", c.flag, c.debug, got, c.want)
		}
	}

	if _, err := resolveLimaLogLevel("bogus", false); err == nil {
		t.Error("expected an error for an invalid level")
	}
}
