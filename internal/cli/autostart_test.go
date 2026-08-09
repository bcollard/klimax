package cli

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestAutostartPlistIsWellFormed(t *testing.T) {
	// A path with XML metacharacters must not break the plist.
	plist := autostartPlist("/usr/local/bin/klimax", "/Users/a&b/<cfg>.yaml", "/tmp/log")

	dec := xml.NewDecoder(strings.NewReader(plist))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, plist)
		}
	}

	if strings.Contains(plist, "/Users/a&b/") {
		t.Errorf("'&' was not escaped:\n%s", plist)
	}
	if !strings.Contains(plist, "/Users/a&amp;b/&lt;cfg&gt;.yaml") {
		t.Errorf("expected escaped config path:\n%s", plist)
	}
	for _, want := range []string{
		"<string>" + autostartLabel + "</string>",
		"<string>up</string>",
		"<string>--config</string>",
		"<key>RunAtLoad</key>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
}
