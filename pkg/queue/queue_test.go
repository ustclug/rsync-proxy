package queue

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquireRejectsWhenQueueIsFull(t *testing.T) {
	q := New(1, 1)

	first := <-q.Acquire()
	require.True(t, first.Ok)

	secondCh := q.Acquire()
	second := <-secondCh
	assert.False(t, second.Ok)
	assert.False(t, second.Full)
	assert.Equal(t, 0, second.Index)
	assert.Equal(t, 1, second.Max)

	third := <-q.Acquire()
	assert.True(t, third.Full)
	assert.False(t, third.Ok)

	q.Release()
	assert.True(t, (<-secondCh).Ok)
	q.Release()
}
