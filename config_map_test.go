package scaffold_test

import (
	"testing"
	"time"

	"github.com/kapetan-io/scaffold"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMapConfigZeroValueReturnsFallbacks(t *testing.T) {
	p := scaffold.MapConfigProvider{}

	_, err := p.String("anything")
	require.Error(t, err)
	assert.Equal(t, "fallback", p.StringOr("anything", "fallback"))

	_, err = p.Int("n")
	require.Error(t, err)
	assert.Equal(t, 42, p.IntOr("n", 42))

	_, err = p.Int64("n")
	require.Error(t, err)
	assert.Equal(t, int64(42), p.Int64Or("n", 42))

	_, err = p.Float64("n")
	require.Error(t, err)
	assert.InDelta(t, 1.5, p.Float64Or("n", 1.5), 1e-9)

	_, err = p.Bool("n")
	require.Error(t, err)
	assert.True(t, p.BoolOr("n", true))

	_, err = p.Duration("n")
	require.Error(t, err)
	assert.Equal(t, time.Second, p.DurationOr("n", time.Second))
}

func TestMapConfigNilValuesMap(t *testing.T) {
	p := scaffold.MapConfigProvider{Values: nil}
	_, err := p.String("anything")
	require.Error(t, err)
	assert.Equal(t, "fallback", p.StringOr("anything", "fallback"))
}

func TestMapConfigPresentAndMissingAndParse(t *testing.T) {
	values := map[string]string{
		"STR":       "hello",
		"INT":       "7",
		"INT64":     "9999999999",
		"FLOAT":     "3.14",
		"BOOL":      "true",
		"DUR":       "250ms",
		"BAD_INT":   "nope",
		"BAD_INT64": "nope",
		"BAD_FLOAT": "nope",
		"BAD_BOOL":  "perhaps",
		"BAD_DUR":   "nope",
	}

	for _, test := range []struct {
		name   string
		assert func(t *testing.T, p *scaffold.MapConfigProvider, captured *testLogHandler)
	}{
		{
			name: "StringPresent",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				v, err := p.String("STR")
				require.NoError(t, err)
				assert.Equal(t, "hello", v)
				assert.Equal(t, "hello", p.StringOr("STR", "fb"))
			},
		},
		{
			name: "StringMissing",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				_, err := p.String("MISSING")
				require.Error(t, err)
				assert.Equal(t, "fb", p.StringOr("MISSING", "fb"))
			},
		},
		{
			name: "IntSuccess",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				v, err := p.Int("INT")
				require.NoError(t, err)
				assert.Equal(t, 7, v)
				assert.Equal(t, 7, p.IntOr("INT", 0))
			},
		},
		{
			name: "Int64Success",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				v, err := p.Int64("INT64")
				require.NoError(t, err)
				assert.Equal(t, int64(9999999999), v)
				assert.Equal(t, int64(9999999999), p.Int64Or("INT64", 0))
			},
		},
		{
			name: "Float64Success",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				v, err := p.Float64("FLOAT")
				require.NoError(t, err)
				assert.InDelta(t, 3.14, v, 1e-9)
				assert.InDelta(t, 3.14, p.Float64Or("FLOAT", 0.0), 1e-9)
			},
		},
		{
			name: "BoolSuccess",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				v, err := p.Bool("BOOL")
				require.NoError(t, err)
				assert.True(t, v)
				assert.True(t, p.BoolOr("BOOL", false))
			},
		},
		{
			name: "DurationSuccess",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				v, err := p.Duration("DUR")
				require.NoError(t, err)
				assert.Equal(t, 250*time.Millisecond, v)
				assert.Equal(t, 250*time.Millisecond, p.DurationOr("DUR", 0))
			},
		},
		{
			name: "IntPlainParseFailure",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				_, err := p.Int("BAD_INT")
				require.Error(t, err)
			},
		},
		{
			name: "IntOrParseFailureLogs",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, captured *testLogHandler) {
				assert.Equal(t, 99, p.IntOr("BAD_INT", 99))
				assert.NotNil(t, captured.findByMessage("config parse failed; using fallback"))
			},
		},
		{
			name: "Int64PlainParseFailure",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				_, err := p.Int64("BAD_INT64")
				require.Error(t, err)
			},
		},
		{
			name: "Int64OrParseFailure",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				assert.Equal(t, int64(99), p.Int64Or("BAD_INT64", 99))
			},
		},
		{
			name: "Float64PlainParseFailure",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				_, err := p.Float64("BAD_FLOAT")
				require.Error(t, err)
			},
		},
		{
			name: "Float64OrParseFailure",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				assert.InDelta(t, 2.5, p.Float64Or("BAD_FLOAT", 2.5), 1e-9)
			},
		},
		{
			name: "BoolPlainParseFailure",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				_, err := p.Bool("BAD_BOOL")
				require.Error(t, err)
			},
		},
		{
			name: "BoolOrParseFailure",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				assert.True(t, p.BoolOr("BAD_BOOL", true))
			},
		},
		{
			name: "DurationPlainParseFailure",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				_, err := p.Duration("BAD_DUR")
				require.Error(t, err)
			},
		},
		{
			name: "DurationOrParseFailure",
			assert: func(t *testing.T, p *scaffold.MapConfigProvider, _ *testLogHandler) {
				assert.Equal(t, time.Minute, p.DurationOr("BAD_DUR", time.Minute))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			log, captured := newTestLogger()
			p := &scaffold.MapConfigProvider{Values: values, Logger: log}
			test.assert(t, p, captured)
		})
	}
}
