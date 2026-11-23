package graph

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoIMAPDependencyInGraphPackage enforces the architectural rule that the
// Graph package knows nothing about IMAP or SMTP. Graph is used for mail today
// and is expected to serve Teams, chats, and files later; every one of those
// would be distorted by an IMAP-shaped seam.
//
// This is a source-level check on purpose: an import-cycle-free build cannot
// express "must not depend on" for a package that is legitimately linked into
// the same binary. A future Graph mailbox that reaches for go-imap must fail
// here, not in review.
func TestNoIMAPDependencyInGraphPackage(t *testing.T) {
	forbidden := []string{
		"github.com/emersion/go-imap",
		"windshift/internal/email", // owns the IMAP client and its FetchedMessage
		"windshift/internal/outbound/smtp",
		"net/smtp",
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
					t.Errorf("%s imports %q: the Graph package must not depend on IMAP or SMTP", name, path)
				}
			}
		}
	}
}
