package scaffold_test

import (
	"context"
	"testing"

	"github.com/kapetan-io/scaffold.go"
	"github.com/stretchr/testify/assert"
)

func TestContextAccessorsReturnNilOnEmptyContext(t *testing.T) {
	ctx := context.Background()
	assert.Nil(t, scaffold.GetConfig(ctx))
	assert.Nil(t, scaffold.GetSecrets(ctx))
	assert.Nil(t, scaffold.GetLogger(ctx))
}
