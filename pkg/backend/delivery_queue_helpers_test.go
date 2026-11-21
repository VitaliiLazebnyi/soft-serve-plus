package backend

// closedForTest reports whether the queue refuses new deliveries, that is,
// whether shutdown has started.
func (q *deliveryQueue) closedForTest() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}
