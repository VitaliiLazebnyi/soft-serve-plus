package common

import (
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
)

// NewSpinner returns the loading spinner every page shows while it waits for
// data.
func (c *Common) NewSpinner() spinner.Model {
	return spinner.New(spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(c.Styles.Spinner))
}

// CopyKey returns the copy key binding with help text describing what it
// copies on the current view.
func (c *Common) CopyKey(help string) key.Binding {
	k := c.KeyMap.Copy
	k.SetHelp("c", help)
	return k
}
