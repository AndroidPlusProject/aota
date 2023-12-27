package aota

import (
	"fmt"
	"io"
)

type PayloadConsumer struct {
        Seekable io.ReadSeekCloser
        Readable io.ReadCloser
}
func (pc PayloadConsumer) Read(p []byte) (int, error) {
        if pc.Seekable != nil {
                return pc.Seekable.Read(p)
        }
        if pc.Readable != nil {
                return pc.Readable.Read(p)
        }
        return 0, fmt.Errorf("payload: No reader")
}
func (pc PayloadConsumer) Seek(offset int64, whence int) (int64, error) {
        if pc.Seekable != nil {
                return pc.Seekable.Seek(offset, whence)
        }
        return 0, fmt.Errorf("payload: No seeker")
}
func (pc PayloadConsumer) Close() error {
        if pc.Seekable != nil {
                if err := pc.Seekable.Close(); err != nil {
                        return err
                }
                pc.Seekable = nil
        }
        if pc.Readable != nil {
                if err := pc.Readable.Close(); err != nil {
                        return err
                }
                pc.Readable = nil
        }
        return nil
}
