package aota

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/dsnet/compress/bzip2"
	xz "github.com/jamespfennell/xz"
)

type InstallOpsPartition struct {
	sync.Mutex //Locks writes to the extracted file

	Name string
	Ops  int
	Size uint64
	File *os.File
}

type InstallOpStep struct {
	Op int
	Iop *InstallOpsPartition
}

type InstallOpsSession struct {
	sync.Mutex //Locks changes to stats to prevent stat confusion

	Threads sync.WaitGroup //Threads remaining
}

type InstallOps struct {
	running bool

	Map     []*InstallOpsPartition //Used to map a raw operation step to a partition
	Ops     []*InstallOperation    //Buffered install operation steps
	Steps   [] InstallOpStep       //Ordered index mapping of install operations
	Session   *InstallOpsSession   //Tracks an ongoing installation

	Payload    *Payload //The raw payload holding these ops
	BlockSize  uint64   //The size to zero-pad install operations to
	BaseOffset uint64   //Offset of install data after header
}

func NewInstallOps(payload *Payload, outDir string) (*InstallOps, error) {
	iops := &InstallOps{
		Map: make([]*InstallOpsPartition, 0),
		Ops: make([]*InstallOperation, 0),
		Steps: make([]InstallOpStep, 0),
		Session: &InstallOpsSession{},
		Payload: payload,
		BlockSize: payload.BlockSize,
		BaseOffset: payload.BaseOffset,
	}

	for i := 0; i < len(payload.Partitions); i++ {
		taskStart := time.Now()

		p := payload.Partitions[i]
		if p.PartitionName == nil {
			continue
		}
		pName := *p.PartitionName

		iop := &InstallOpsPartition{
			Name: pName,
			Ops: len(p.Operations),
		}
		for j := 0; j < len(p.Operations); j++ {
			iop.Size += *p.Operations[j].DataLength
			iops.Steps = append(iops.Steps, InstallOpStep{Op: j, Iop: iop})
		}

		outFile := fmt.Sprintf("%s/%s.img", outDir, iop.Name)
		file, err := os.Create(outFile)
		if err != nil {
			return nil, fmt.Errorf("Failed to create %s: %v\n", outFile, err)
		}
		iop.File = file

		iops.Map = append(iops.Map, iop)
		iops.Ops = append(iops.Ops, p.Operations...)

		if iops.Payload.InstallSession.Debug {
			fmt.Printf("Task time: %dms\n", time.Now().Sub(taskStart).Milliseconds())
		}
	}

	return iops, nil
}

func (iops *InstallOps) Run() {
	if iops.running {
		return
	}
	if len(iops.Map) == 0 || len(iops.Ops) == 0 {
		return
	}
	iops.running = true
	for i := 0; i < len(iops.Steps); i++ {
		iops.Session.Threads.Add(1)
		go func(op int) {
			defer iops.Session.Threads.Done()
			iops.writeOp(op)
		}(i)
	}
	iops.Session.Threads.Wait()
	iops.running = false
}

func (iops *InstallOps) writeOp(opIndex int) {
	step := iops.Steps[opIndex]
	op := step.Op
	iop := step.Iop
	ioop := iops.Ops[op]
	seek := int64(iops.BaseOffset + *ioop.DataOffset)
	dataLength := *ioop.DataLength

	//Reserve enough free memory
	iops.Payload.InstallSession.reserve(dataLength)

	//Read the bytes for the operation
	data, err := iops.Payload.ReadBytes(seek, int64(dataLength))
	if err != nil {
		fmt.Printf("Failed to read %d bytes from offset %d in payload for %s op %d: %v\n", dataLength, seek, iop.Name, op, err)
		return
	}

	//Write the bytes for the operation
	iop.Lock()
	partSeek := int64(*ioop.DstExtents[0].StartBlock * iops.BlockSize)
	if _, err := iop.File.Seek(partSeek, 0); err != nil {
		fmt.Printf("Failed to seek to %d in output for %s op %d: %v\n", partSeek, iop.Name, op, err)
		return
	}

	switch *ioop.Type {
	case InstallOperation_REPLACE:
		if _, err := iop.File.Write(data); err != nil {
			fmt.Printf("Failed to write output for %s op %d: %v\n", iop.Name, op, err)
			return
		}
	case InstallOperation_REPLACE_BZ:
		bzr, err := bzip2.NewReader(bytes.NewReader(data), nil)
		if err != nil {
			fmt.Printf("Failed to init bzip2 for %s op %d: %v\n", iop.Name, op, err)
			return
		}
		if _, err := io.Copy(iop.File, bzr); err != nil {
			fmt.Printf("Failed to write bzip2 output for %s op %d: %v\n", iop.Name, op, err)
			return
		}
	case InstallOperation_REPLACE_XZ:
		xzr := xz.NewReader(bytes.NewReader(data))
		if _, err := io.Copy(iop.File, xzr); err != nil {
			fmt.Printf("Failed to write xz output for %s op %d: %v\n", iop.Name, op, err)
			return
		}
	case InstallOperation_ZERO:
		for _, ext := range ioop.DstExtents {
			partSeek = int64(*ext.StartBlock * iops.BlockSize)
			if _, err := iop.File.Seek(partSeek, 0); err != nil {
				fmt.Printf("Failed to seek to %d in output for %s op %d: %v\n", partSeek, iop.Name, op, err)
				os.Exit(1)
			}
			if _, err := io.Copy(iop.File, bytes.NewReader(make([]byte, *ext.NumBlocks*iops.BlockSize))); err != nil {
				fmt.Printf("Failed to write zero output for %s op %d: %v\n", iop.Name, op, err)
				os.Exit(1)
			}
		}
	default:
		fmt.Printf("Unsupported operation type %d (%s), please report a bug!\n", *ioop.Type, InstallOperation_Type_name[int32(*ioop.Type)])
		os.Exit(1)
	}
	iop.Unlock()

	//Free the reserved memory
	iops.Payload.InstallSession.free(dataLength)
}
