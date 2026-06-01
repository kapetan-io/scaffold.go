package scaffold_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kapetan-io/scaffold.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanerLIFO(t *testing.T) {
	c := scaffold.NewCleaner(discardLogger())
	var order []string
	c.Add(func(context.Context) error {
		order = append(order, "first")
		return nil
	})
	c.Add(func(context.Context) error {
		order = append(order, "second")
		return nil
	})
	c.Add(func(context.Context) error {
		order = append(order, "third")
		return nil
	})

	require.NoError(t, c.Clean(context.Background()))
	assert.Equal(t, []string{"third", "second", "first"}, order)
}

func TestCleanerPanicRecovery(t *testing.T) {
	log, captured := newTestLogger()
	c := scaffold.NewCleaner(log)
	var ran []string
	c.Add(func(context.Context) error {
		ran = append(ran, "first")
		return nil
	})
	c.Add(func(context.Context) error {
		panic("middle boom")
	})
	c.Add(func(context.Context) error {
		ran = append(ran, "last")
		return nil
	})

	require.NoError(t, c.Clean(context.Background()))
	assert.Equal(t, []string{"last", "first"}, ran)

	rec := captured.findByMessage("cleaner function panicked")
	require.NotNil(t, rec)
	panicAttr, ok := recordAttr(*rec, "panic")
	require.True(t, ok)
	assert.Equal(t, "middle boom", panicAttr.Value.String())
	_, hasStack := recordAttr(*rec, "stack")
	assert.True(t, hasStack)
}

func TestCleanerErrorContinuation(t *testing.T) {
	log, captured := newTestLogger()
	c := scaffold.NewCleaner(log)
	var ran []string
	c.Add(func(context.Context) error {
		ran = append(ran, "first")
		return nil
	})
	c.Add(func(context.Context) error {
		return errors.New("middle broke")
	})
	c.Add(func(context.Context) error {
		ran = append(ran, "last")
		return nil
	})

	require.NoError(t, c.Clean(context.Background()))
	assert.Equal(t, []string{"last", "first"}, ran)

	rec := captured.findByMessage("cleaner function returned error")
	require.NotNil(t, rec)
	errAttr, ok := recordAttr(*rec, "error")
	require.True(t, ok)
	assert.Contains(t, errAttr.Value.String(), "middle broke")
}

func TestCleanerAddAfterClean(t *testing.T) {
	c := scaffold.NewCleaner(discardLogger())
	c.Add(func(context.Context) error { return nil })
	require.NoError(t, c.Clean(context.Background()))

	assert.Panics(t, func() {
		c.Add(func(context.Context) error { return nil })
	})
}

func TestCleanerDoubleClean(t *testing.T) {
	c := scaffold.NewCleaner(discardLogger())
	count := 0
	c.Add(func(context.Context) error {
		count++
		return nil
	})

	require.NoError(t, c.Clean(context.Background()))
	assert.Equal(t, 1, count)

	err := c.Clean(context.Background())
	require.Error(t, err)
	assert.Equal(t, 1, count)
}

func TestCleanerConstructionPanicsOnNilLogger(t *testing.T) {
	assert.Panics(t, func() {
		scaffold.NewCleaner(nil)
	})
}
