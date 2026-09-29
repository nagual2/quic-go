//go:build !linux

// Package io_uring is a Linux-only experimental send path; this stub keeps
// dependent packages compiling on other platforms.
package io_uring

import (
	"errors"
	"net"
)

const ringEntries = 64

// Sender is a no-op placeholder outside Linux.
type Sender struct{}

func NewSender(fd int) (*Sender, error) {
	return nil, errors.New("io_uring send path requires Linux")
}

func (s *Sender) Close() error { return nil }

func (s *Sender) Submit(data []byte, addr *net.UDPAddr, control []byte) error {
	return errors.New("io_uring send path requires Linux")
}

func (s *Sender) Flush() error { return nil }
