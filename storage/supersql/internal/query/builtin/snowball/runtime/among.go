// Source: https://raw.githubusercontent.com/snowballstem/snowball/d19326ac6c1b9a417fc872f7c2f845265a5e9ece/go/among.go
// Snowball BSD-3-Clause license: ../LICENSE.SNOWBALL.
package snowball

type AmongF func(env *Env, ctx interface{}) bool

type Among struct {
	Str string
	A   int32
	B   int32
	F   AmongF
}
