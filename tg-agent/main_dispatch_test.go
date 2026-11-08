package main

import (
	"context"
	"testing"
	"time"
)

func TestDispatcherSaturatedWorkKeepsApprovalsAndStopsUsable(t *testing.T) {
	c := runtimeTestConfig(t)
	ordinaryRelease := make(chan struct{})
	controlRelease := make(chan struct{})
	ordinaryStarted := make(chan struct{}, 16)
	approvalStarted := make(chan struct{}, 2)
	dispatcher := newUpdateDispatcher(16, 2, func(Message) { ordinaryStarted <- struct{}{}; <-ordinaryRelease }, func(CallbackQuery) { approvalStarted <- struct{}{}; <-controlRelease })
	defer func() { close(ordinaryRelease); close(controlRelease); dispatcher.wait() }()
	task := Update{Message: &Message{From: User{ID: c.OwnerID}, Chat: Chat{ID: c.OwnerID, Type: "private"}, Text: "long task"}}
	for i := 0; i < 16; i++ {
		if !dispatcher.dispatch(task) {
			t.Fatalf("task %d unexpectedly rejected", i)
		}
	}
	for i := 0; i < 16; i++ {
		select {
		case <-ordinaryStarted:
		case <-time.After(time.Second):
			t.Fatal("ordinary handler did not start")
		}
	}
	admission := make(chan bool, 1)
	go func() { admission <- dispatcher.dispatch(task) }()
	select {
	case accepted := <-admission:
		if accepted {
			t.Fatal("17th task exceeded ordinary limit")
		}
	case <-time.After(time.Second):
		t.Fatal("saturated admission blocked polling")
	}
	approval := Update{CallbackQuery: &CallbackQuery{ID: "a", From: User{ID: c.OwnerID}, Message: &Message{Chat: Chat{ID: c.OwnerID, Type: "private"}}, Data: "approve:nonce"}}
	for i := 0; i < 2; i++ {
		if !dispatcher.dispatch(approval) {
			t.Fatal("ordinary saturation blocked approval lane")
		}
		select {
		case <-approvalStarted:
		case <-time.After(time.Second):
			t.Fatal("approval did not reach independent lane")
		}
	}
	ctx, finish, ok := beginTask(c.OwnerID)
	if !ok {
		t.Fatal("could not register cancellation fixture")
	}
	defer finish()
	// Another user cannot turn a forged private-chat update into a stop signal.
	forged := Update{Message: &Message{From: User{ID: c.OwnerID + 1}, Chat: Chat{ID: c.OwnerID, Type: "private"}, Text: "/stop"}}
	if dispatcher.dispatch(forged) {
		t.Fatal("forged stop occupied reserved control lane")
	}
	if ctx.Err() != nil {
		t.Fatal("forged stop canceled someone else's task")
	}
	stop := Update{Message: &Message{From: User{ID: c.OwnerID}, Chat: Chat{ID: c.OwnerID, Type: "private"}, Text: "/stop@agent_bot"}}
	// Network/control handlers are full too; local cancellation must still happen.
	if dispatcher.dispatch(stop) {
		t.Fatal("control limit was not enforced")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("stop did not cancel immediately: %v", ctx.Err())
	}
}
