package scaffold_test

import (
	"testing"

	"github.com/kapetan-io/scaffold.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBindingsDuplicateAddPanics(t *testing.T) {
	var b scaffold.DefaultBindings
	b.Add("api", 8080)
	require.PanicsWithValue(t,
		`scaffold: Bindings.Add: duplicate binding name "api"`,
		func() { b.Add("api", 9090) })
}

func TestBindingsGetReturnsNilForMissing(t *testing.T) {
	var b scaffold.DefaultBindings
	assert.Nil(t, b.Get("missing"))

	b.Add("api", 8080)
	assert.NotNil(t, b.Get("api"))
	assert.Nil(t, b.Get("other"))
}

func TestTestBindingsDuplicateAddPanics(t *testing.T) {
	var b scaffold.TestBindings
	b.Add("api", 0)
	require.PanicsWithValue(t,
		`scaffold: Bindings.Add: duplicate binding name "api"`,
		func() { b.Add("api", 0) })
}

func TestTestBindingsGetReturnsNilForMissing(t *testing.T) {
	var b scaffold.TestBindings
	assert.Nil(t, b.Get("missing"))

	b.Add("api", 0)
	assert.NotNil(t, b.Get("api"))
}
