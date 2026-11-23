package teams

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoIMAPOrMailboxDependency enforces that this package stays a capability of
// its own. Teams is not a mailbox: a channel has no folders and no UID space, so
// reaching for the mailbox contract or the IMAP client would mean the design had
// been bent to fit transport that does not exist here.
func TestNoIMAPOrMailboxDependency(t *testing.T) {
	forbidden := []string{
		"github.com/emersion/go-imap",
		"net/smtp",
		"windshift/internal/email",
		"windshift/internal/mailbox",
		"windshift/internal/mailmsg",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", name, err)
			}
			for _, banned := range forbidden {
				if path == banned || strings.HasPrefix(path, banned+"/") {
					t.Errorf("%s imports %q: the Teams capability must not depend on IMAP, SMTP, or the mailbox contract", name, path)
				}
			}
		}
	}
}
