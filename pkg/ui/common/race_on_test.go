//go:build race

package common

// raceEnabled is set when the tests run with the race detector, which makes
// rendering several times slower.
const raceEnabled = true
