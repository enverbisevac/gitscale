package main

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

type multiWriter struct {
	writers []io.Writer
}

type ErrWrite struct {
	ID  int
	Err error
}

func (e *ErrWrite) Error() string {
	return fmt.Sprintf("write error %v at index: %d", e.Err, e.ID)
}

func (t *multiWriter) Write(p []byte) (int, error) {
	errs := make([]error, 0, len(t.writers))
	for i, w := range t.writers {
		n, err := w.Write(p)
		if err != nil {
			errs = append(errs, &ErrWrite{
				ID:  i,
				Err: err,
			})
			continue
		}
		if n != len(p) {
			errs = append(errs, &ErrWrite{
				ID:  i,
				Err: io.ErrShortWrite,
			})
			continue
		}
	}

	if len(errs) > 0 {
		return len(p), errors.Join(errs...)
	}

	return len(p), nil
}

// AsyncMultiWriter behaves similarly as io.MultiWriter, but the write on each writer
// is run concurrently.
// Writing on every writer is attempted, regardless of any errors returned from
// any of the writers.
// The n returned is the sum of number of bytes for all the writes performed.
// When at least one of the writers returns an error, AsyncMultiWriter returns the error.
// The error returned is not necessarily from the first writer that would result in error.
func AsyncMultiWriter(writers ...io.Writer) io.Writer {
	allWriters := make([]io.Writer, 0, len(writers))
	for _, w := range writers {
		if mw, ok := w.(*asyncMultiWriter); ok {
			allWriters = append(allWriters, mw.writers...)
		} else {
			allWriters = append(allWriters, w)
		}
	}
	return &asyncMultiWriter{allWriters}
}

type asyncMultiWriter struct {
	writers []io.Writer
}

func (aw *asyncMultiWriter) Write(p []byte) (int, error) {
	var wg sync.WaitGroup
	wg.Add(len(aw.writers))

	type nAndErrType struct {
		n   int
		err error
	}

	nAndErrs := make([]nAndErrType, len(aw.writers))

	for i, w := range aw.writers {
		go func(i int, w io.Writer) {
			nAndErrs[i].n, nAndErrs[i].err = w.Write(p)
			wg.Done()
		}(i, w)
	}

	wg.Wait()

	errs := make([]error, 0, len(aw.writers))
	for _, val := range nAndErrs {
		if val.err != nil {
			errs = append(errs, val.err)
		}
	}

	if len(errs) > 0 {
		return len(p), errors.Join(errs...)
	}

	return len(p), nil
}

// MultiWriter creates a writer that duplicates its writes to all the
// provided writers, similar to the Unix tee(1) command.
//
// Each write is written to each listed writer, one at a time.
// If a listed writer returns an error, that overall write operation
// stops and returns the error; it does not continue down the list.
func MultiWriter(writers ...io.Writer) io.Writer {
	allWriters := make([]io.Writer, 0, len(writers))
	for _, w := range writers {
		if mw, ok := w.(*multiWriter); ok {
			allWriters = append(allWriters, mw.writers...)
		} else {
			allWriters = append(allWriters, w)
		}
	}
	return &multiWriter{allWriters}
}
