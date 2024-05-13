package aota

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type InstallSession struct {
	Payloads []*Payload
	Usage       InstallSessionUsage
	Debug       bool

	closed     bool
	installing bool
}

type InstallSessionUsage struct {
	sync.Mutex

	limit uint64
	total uint64
}

func NewInstallSession(input string, extract []string, outDir string) (*InstallSession, error) {
	if input == "" {
		return nil, fmt.Errorf("must provide at least one payload as input")
	}

	is := &InstallSession{
		Payloads: make([]*Payload, 0),
	}

	if err := is.AddPayload(input, extract, outDir); err != nil {
		return nil, fmt.Errorf("error adding payload (%s): %v", input, err)
	}
	return is, nil
}

func NewInstallSessionMulti(input, extract []string, outDir string) (*InstallSession, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("must provide at least one payload as input")
	}
	is, err := NewInstallSession(input[0], extract, outDir)
	if err != nil {
		return nil, err
	}
	for i := 1; i < len(input); i++ {
		if err := is.AddPayload(input[i], extract, outDir); err != nil {
			return nil, err
		}
	}
	return is, nil
}

func (is *InstallSession) AddPayload(in string, extract []string, outDir string) (err error) {
	var payload *Payload
	if strings.HasPrefix(in, "http://") || strings.HasPrefix(in, "https://") {
		payload, err = NewPayloadURL(in, extract)
		if err != nil {
			return
		}
	} else {
		payload, err = NewPayloadFile(in, extract, true)
		if err != nil {
			return
		}
	}
	payload.InstallSession = is

	iops, err := NewInstallOps(payload, outDir)
	if err != nil {
		return err
	}
	payload.Installer = iops

	is.Payloads = append(is.Payloads, payload)
	return
}

func (is *InstallSession) Install() {
	if is.installing || is.closed {
		return
	}
	is.installing = true
	defer is.Close()
	for i := 0; i < len(is.Payloads); i++ {
		defer is.Payloads[i].Close()
		is.Payloads[i].Installer.Run()
	}
}

func (is *InstallSession) IsInstalling() bool {
	return is.installing
}

func (is *InstallSession) SetMemoryLimit(i uint64) {
	is.Usage.limit = i
}

func (is *InstallSession) reserve(i uint64) {
	for {
		is.Usage.Lock()
		if is.Usage.total + i < is.Usage.limit || is.Usage.total <= 0 {
			is.Usage.total += i
			is.Usage.Unlock()
			return
		}
		is.Usage.Unlock()
		time.Sleep(time.Millisecond * 100)
	}
}

func (is *InstallSession) free(i uint64) {
	is.Usage.Lock()
	is.Usage.total -= i
	if is.Usage.total < 0 {
		is.Usage.total = 0
	}
	is.Usage.Unlock()
}

func (is *InstallSession) SetDebug(b bool) {
	is.Debug = b
}

func (is *InstallSession) Close() {
	is.installing = false
	is.closed = true
}

func (is *InstallSession) IsClosed() bool {
	return is.closed
}
