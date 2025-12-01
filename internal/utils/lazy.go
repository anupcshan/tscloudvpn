package utils

import (
	"sync"
	"time"
)

func LazyWithErrors[T any](f func() (T, error)) func() T {
	return sync.OnceValue(
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
