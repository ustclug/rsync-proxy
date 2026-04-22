package queue

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAcquireRejectsWhenQueueIsFull(t *testing.T) {
	q := New(1, 1)

	h1 := q.Acquire()
	first := <-h1.C
	require.True(t, first.Ok)

	h2 := q.Acquire()
	second := <-h2.C
	assert.False(t, second.Ok)
	assert.False(t, second.Full)
	assert.Equal(t, 0, second.Index)
	assert.Equal(t, 1, second.Max)

	h3 := q.Acquire()
	third := <-h3.C
	assert.True(t, third.Full)
	assert.False(t, third.Ok)

	h1.Release()
	assert.True(t, (<-h2.C).Ok)
	h2.Release()
}
