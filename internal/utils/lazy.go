package utils

import (
	"time"

	"github.com/bradenaw/juniper/xsync"
)

func LazyWithErrors[T any](f func() (T, error)) func() T {
	return xsync.Lazy(
		func() T {
			for {
				v, err := f()
				if err == nil {
					return v
				}
				// TODO: Backoff instead of tight polling
				time.Sleep(30 * time.Second)
			}
		},
	)
}
