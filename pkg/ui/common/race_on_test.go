//go:build race

package common

// raceEnabled is set when the tests run with the race detector, which makes
// chroma's analysers some twenty times slower.
const raceEnabled = true
