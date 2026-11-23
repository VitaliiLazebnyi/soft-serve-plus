package ui

import (
	"os"
	"strconv"

	"github.com/charmbracelet/colorprofile"
)

// ColorProfile returns the color profile of the TUI over SSH: 256 colors,
// or none when SOFT_SERVE_NO_COLOR is true.
func ColorProfile() colorprofile.Profile {
	if noColor, _ := strconv.ParseBool(os.Getenv("SOFT_SERVE_NO_COLOR")); noColor {
		return colorprofile.NoTTY
	}
	return colorprofile.ANSI256
}
