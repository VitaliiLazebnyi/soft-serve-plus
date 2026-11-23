package syntax

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/soft-serve/pkg/ui/styles"
)

// FormatLineNumber adds line numbers to a string.
func FormatLineNumber(styles *styles.Styles, s string, color bool) (string, int) {
	lines := strings.Split(s, "\n")
	// NB: len() is not a particularly safe way to count string width (because
	// it's counting bytes instead of runes) but in this case it's okay
	// because we're only dealing with digits, which are one byte each.
	mll := len(fmt.Sprintf("%d", len(lines)))
	for i, l := range lines {
		digit := fmt.Sprintf("%*d", mll, i+1)
		bar := "│"
		if color {
			digit = styles.Code.LineDigit.Render(digit)
			bar = styles.Code.LineBar.Render(bar)
		}
		if i < len(lines)-1 || len(l) != 0 {
			// If the final line was a newline we'll get an empty string for
			// the final line, so drop the newline altogether.
			lines[i] = fmt.Sprintf(" %s %s %s", digit, bar, l)
		}
	}
	return strings.Join(lines, "\n"), mll
}

// FormatHighlight adds syntax highlighting to c, the file at p, without a
// margin. Highlighting is bounded as Highlight bounds it; when it would take
// too long, c is returned as is.
func FormatHighlight(p, c string, budget Budget) (string, error) {
	st := StyleConfig()
	if out, ok := Highlight(st, p, c, true, budget); ok {
		return out, nil
	}
	return c, nil
}
