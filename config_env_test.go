package scaffold_test

import (
	"testing"
	"time"

	"github.com/kapetan-io/scaffold.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvConfigStringPresentAndMissing(t *testing.T) {
	p := &scaffold.EnvConfigProvider{}

	// Missing key.
	_, err := p.String("SCAFFOLD_MISSING_KEY")
	require.Error(t, err)
	require.ErrorContains(t, err, "SCAFFOLD_MISSING_KEY")
	assert.Equal(t, "fallback", p.StringOr("SCAFFOLD_MISSING_KEY", "fallback"))

	// Present key — even empty values count as present.
	t.Setenv("SCAFFOLD_PRESENT", "value")
	v, err := p.String("SCAFFOLD_PRESENT")
	require.NoError(t, err)
	assert.Equal(t, "value", v)
	assert.Equal(t, "value", p.StringOr("SCAFFOLD_PRESENT", "fallback"))

	t.Setenv("SCAFFOLD_EMPTY", "")
	v, err = p.String("SCAFFOLD_EMPTY")
	require.NoError(t, err)
	assert.Equal(t, "", v)
}

func TestEnvConfigIntParseFailurePlainReturnsError(t *testing.T) {
	p := &scaffold.EnvConfigProvider{}
	t.Setenv("SCAFFOLD_BAD_INT", "not-a-number")

	_, err := p.Int("SCAFFOLD_BAD_INT")
	require.Error(t, err)
	require.ErrorContains(t, err, "SCAFFOLD_BAD_INT")
}

func TestEnvConfigIntParseFailureOrLogsAndFallsBack(t *testing.T) {
	log, captured := newTestLogger()
	p := &scaffold.EnvConfigProvider{Logger: log}
	t.Setenv("SCAFFOLD_BAD_INT", "not-a-number")

	got := p.IntOr("SCAFFOLD_BAD_INT", 42)
	assert.Equal(t, 42, got)

	r := captured.findByMessage("config parse failed; using fallback")
	require.NotNil(t, r)

	key, ok := recordAttr(*r, "key")
	require.True(t, ok)
	assert.Equal(t, "SCAFFOLD_BAD_INT", key.Value.String())
}

func TestEnvConfigAllNumericAndBoolAndDuration(t *testing.T) {
	const key = "SCAFFOLD_TYPED_VALUE"

	for _, test := range []struct {
		name   string
		raw    string
		assert func(t *testing.T, p *scaffold.EnvConfigProvider)
	}{
		{
			name: "IntPlainSuccess",
			raw:  "7",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				v, err := p.Int(key)
				require.NoError(t, err)
				assert.Equal(t, 7, v)
			},
		},
		{
			name: "IntOrSuccess",
			raw:  "7",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				assert.Equal(t, 7, p.IntOr(key, 1))
			},
		},
		{
			name: "Int64PlainSuccess",
			raw:  "9999999999",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				v, err := p.Int64(key)
				require.NoError(t, err)
				assert.Equal(t, int64(9999999999), v)
			},
		},
		{
			name: "Int64OrSuccess",
			raw:  "9999999999",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				assert.Equal(t, int64(9999999999), p.Int64Or(key, 1))
			},
		},
		{
			name: "Float64PlainSuccess",
			raw:  "3.14",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				v, err := p.Float64(key)
				require.NoError(t, err)
				assert.InDelta(t, 3.14, v, 1e-9)
			},
		},
		{
			name: "Float64OrSuccess",
			raw:  "3.14",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				assert.InDelta(t, 3.14, p.Float64Or(key, 1.0), 1e-9)
			},
		},
		{
			name: "BoolPlainSuccess",
			raw:  "true",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				v, err := p.Bool(key)
				require.NoError(t, err)
				assert.True(t, v)
			},
		},
		{
			name: "BoolOrSuccess",
			raw:  "true",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				assert.True(t, p.BoolOr(key, false))
			},
		},
		{
			name: "DurationPlainSuccess",
			raw:  "250ms",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				v, err := p.Duration(key)
				require.NoError(t, err)
				assert.Equal(t, 250*time.Millisecond, v)
			},
		},
		{
			name: "DurationOrSuccess",
			raw:  "250ms",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				assert.Equal(t, 250*time.Millisecond, p.DurationOr(key, time.Second))
			},
		},
		{
			name: "Int64PlainParseFailure",
			raw:  "not-a-number",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				_, err := p.Int64(key)
				require.Error(t, err)
			},
		},
		{
			name: "Float64PlainParseFailure",
			raw:  "abc",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				_, err := p.Float64(key)
				require.Error(t, err)
			},
		},
		{
			name: "BoolPlainParseFailure",
			raw:  "perhaps",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				_, err := p.Bool(key)
				require.Error(t, err)
			},
		},
		{
			name: "DurationPlainParseFailure",
			raw:  "not-a-duration",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				_, err := p.Duration(key)
				require.Error(t, err)
			},
		},
		{
			name: "Int64OrParseFailure",
			raw:  "not-a-number",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				assert.Equal(t, int64(99), p.Int64Or(key, 99))
			},
		},
		{
			name: "Float64OrParseFailure",
			raw:  "abc",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				assert.InDelta(t, 1.5, p.Float64Or(key, 1.5), 1e-9)
			},
		},
		{
			name: "BoolOrParseFailure",
			raw:  "perhaps",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				assert.True(t, p.BoolOr(key, true))
			},
		},
		{
			name: "DurationOrParseFailure",
			raw:  "not-a-duration",
			assert: func(t *testing.T, p *scaffold.EnvConfigProvider) {
				assert.Equal(t, time.Minute, p.DurationOr(key, time.Minute))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(key, test.raw)
			log, _ := newTestLogger()
			p := &scaffold.EnvConfigProvider{Logger: log}
			test.assert(t, p)
		})
	}
}
