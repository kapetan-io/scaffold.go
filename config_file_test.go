package scaffold_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kapetan-io/scaffold.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileConfigReadsFromDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "DB_PASSWORD"), []byte("s3cret"), 0o600))

	p := &scaffold.FileConfigProvider{Dir: dir}
	v, err := p.String("DB_PASSWORD")
	require.NoError(t, err)
	assert.Equal(t, "s3cret", v)
}

func TestFileConfigMissingFileReturnsNotFound(t *testing.T) {
	p := &scaffold.FileConfigProvider{Dir: t.TempDir()}

	_, err := p.String("MISSING")
	require.Error(t, err)
	require.ErrorContains(t, err, "MISSING")
	assert.Equal(t, "fallback", p.StringOr("MISSING", "fallback"))
}

func TestFileConfigRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	p := &scaffold.FileConfigProvider{Dir: dir}

	for _, test := range []struct {
		name string
		key  string
	}{
		{name: "DotDot", key: ".."},
		{name: "ForwardSlash", key: "/etc/passwd"},
		{name: "NestedDotDot", key: "dir/../key"},
		{name: "NestedPath", key: "dir/key"},
		{name: "LeadingSlash", key: "/key"},
		{name: "TrailingSlash", key: "key/"},
		{name: "SingleDot", key: "."},
		{name: "Empty", key: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := p.String(test.key)
			require.Error(t, err)
		})
	}
}

func TestFileConfigTrimsSingleTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "UNIX"), []byte("hello\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "DOS"), []byte("hello\r\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "DOUBLE"), []byte("hello\n\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "NONE"), []byte("hello"), 0o600))

	p := &scaffold.FileConfigProvider{Dir: dir}

	v, err := p.String("UNIX")
	require.NoError(t, err)
	assert.Equal(t, "hello", v)

	v, err = p.String("DOS")
	require.NoError(t, err)
	assert.Equal(t, "hello", v)

	v, err = p.String("DOUBLE")
	require.NoError(t, err)
	assert.Equal(t, "hello\n", v)

	v, err = p.String("NONE")
	require.NoError(t, err)
	assert.Equal(t, "hello", v)
}

func TestFileConfigPreservesInternalNewlinesForPEM(t *testing.T) {
	const pem = "-----BEGIN CERTIFICATE-----\nAAAA\nBBBB\n-----END CERTIFICATE-----\n"
	const want = "-----BEGIN CERTIFICATE-----\nAAAA\nBBBB\n-----END CERTIFICATE-----"

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "TLS_CRT"), []byte(pem), 0o600))

	p := &scaffold.FileConfigProvider{Dir: dir}
	v, err := p.String("TLS_CRT")
	require.NoError(t, err)
	assert.Equal(t, want, v)
}

func TestFileConfigRereadsOnEveryCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ROTATING")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o600))

	p := &scaffold.FileConfigProvider{Dir: dir}
	v, err := p.String("ROTATING")
	require.NoError(t, err)
	assert.Equal(t, "v1", v)

	require.NoError(t, os.WriteFile(path, []byte("v2"), 0o600))
	v, err = p.String("ROTATING")
	require.NoError(t, err)
	assert.Equal(t, "v2", v)
}
