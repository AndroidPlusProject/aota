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
		if p.PartitionName == "" {
			continue
		}
		pName := p.PartitionName

		iop := &InstallOpsPartition{
			Name: pName,
			Ops: len(p.Operations),
		}
		for j := 0; j < len(p.Operations); j++ {
			iop.Size += p.Operations[j].GetDataLength()
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

func (iops *InstallOps) checkDelta(required uint32, opType InstallOperation_Type) {
	if delta := iops.Payload.Manifest.GetMinorVersion(); delta > 0 && delta < required {
		fmt.Printf("Unexpected op type %s on delta %d, expected %d\n", opType, delta, required)
		os.Exit(1)
	}
}

func (iops *InstallOps) writeOp(opIndex int) {
	step := iops.Steps[opIndex]
	op := step.Op
	iop := step.Iop
	ioop := iops.Ops[op]
	seek := int64(iops.BaseOffset + ioop.GetDataOffset())
	dataLength := ioop.GetDataLength()

	//Reserve enough free memory
	iops.Payload.InstallSession.reserve(dataLength)

	//Read the bytes for the operation
	data, err := iops.Payload.ReadBytes(seek, int64(dataLength))
	if err != nil {
		fmt.Printf("Failed to read %d bytes from offset %d in payload for %s op %d: %v\n", dataLength, seek, iop.Name, op, err)
		os.Exit(1)
		return
	}

	//Write the bytes for the operation
	iop.Lock()
	defer iop.Unlock()
	partSeek := int64(ioop.DstExtents[0].GetStartBlock() * iops.BlockSize)
	if _, err := iop.File.Seek(partSeek, 0); err != nil {
		fmt.Printf("Failed to seek to %d in output for %s op %d: %v\n", partSeek, iop.Name, op, err)
		os.Exit(1)
		return
	}

	switch ioop.Type {

	// Write zeros in the destination.
	case InstallOperation_ZERO: //delta >= 4
		iops.checkDelta(4, ioop.Type)
		for _, ext := range ioop.DstExtents {
			partSeek = int64(ext.GetStartBlock() * iops.BlockSize)
			if _, err := iop.File.Seek(partSeek, 0); err != nil {
				fmt.Printf("Failed to seek to %d in output for %s op %d: %v\n", partSeek, iop.Name, op, err)
				os.Exit(1)
			}
			w := bytes.NewBuffer(make([]byte, ext.GetNumBlocks()*iops.BlockSize))
			defer w.Reset()
			if _, err := io.Copy(iop.File, w); err != nil {
				fmt.Printf("Failed to write zero output for %s op %d: %v\n", iop.Name, op, err)
				os.Exit(1)
			}
		}

	// Discard the destination blocks, reading as undefined.
	case InstallOperation_DISCARD: //delta >= 4
		iops.checkDelta(4, ioop.Type)
		fmt.Println("TODO: DISCARD")

	// Deprecated: Move source extents to target extents.
	case InstallOperation_MOVE: //delta >= 1
		iops.checkDelta(1, ioop.Type)
		fmt.Println("TODO: MOVE")

	// Copy from source to target partition.
	case InstallOperation_SOURCE_COPY: //delta >= 2
		iops.checkDelta(2, ioop.Type)
		//A/B devices need to copy from source to target slot
		//For now, act like an A-only device and update in-place to the target images
		return

	// Replace destination extents w/ attached data.
	case InstallOperation_REPLACE: //delta >= 1
		iops.checkDelta(1, ioop.Type)
		if _, err := iop.File.Write(data); err != nil {
			fmt.Printf("Failed to write output for %s op %d: %v\n", iop.Name, op, err)
			os.Exit(1)
			return
		}

	// Replace destination extents w/ attached bzipped data.
	case InstallOperation_REPLACE_BZ: //delta >= 1
		iops.checkDelta(1, ioop.Type)
		bzr, err := bzip2.NewReader(bytes.NewReader(data), nil)
		if err != nil {
			fmt.Printf("Failed to init bzip2 for %s op %d: %v\n", iop.Name, op, err)
			os.Exit(1)
			return
		}
		defer bzr.Close()
		if _, err := io.Copy(iop.File, bzr); err != nil {
			fmt.Printf("Failed to write bzip2 output for %s op %d: %v\n", iop.Name, op, err)
			os.Exit(1)
			return
		}

	// Replace destination extents w/ attached xz data.
	case InstallOperation_REPLACE_XZ: //delta >= 3
		iops.checkDelta(3, ioop.Type)
		xzr := xz.NewReader(bytes.NewReader(data))
		defer xzr.Close()
		if _, err := io.Copy(iop.File, xzr); err != nil {
			fmt.Printf("Failed to write xz output for %s op %d: %v\n", iop.Name, op, err)
			os.Exit(1)
			return
		}

	// Deprecated: The data is a bsdiff binary diff.
	case InstallOperation_BSDIFF: //delta >= 1
		iops.checkDelta(1, ioop.Type)
		fmt.Println("TODO: BSDIFF")

	// Like BSDIFF, but read from source partition.
	case InstallOperation_SOURCE_BSDIFF: //delta >= 2
		iops.checkDelta(2, ioop.Type)
		fmt.Println("TODO: SOURCE BSDIFF")

	// Like SOURCE_BSDIFF, but compressed with brotli.
	case InstallOperation_BROTLI_BSDIFF: //delta >= 4
		iops.checkDelta(4, ioop.Type)
		fmt.Println("TODO: BROTLI BSDIFF")

	// The data is in puffdiff format.
	case InstallOperation_PUFFDIFF: //delta >= 5
		iops.checkDelta(5, ioop.Type)
		fmt.Println("TODO: PUFFDIFF")

	// The data is in zucchini format.
	case InstallOperation_ZUCCHINI: //delta >= 8
		iops.checkDelta(8, ioop.Type)
		fmt.Println("TODO: ZUCCHINI")

	// Like BSDIFF, but compressed with lzma4.
	case InstallOperation_LZ4DIFF_BSDIFF: //delta >= 9
		iops.checkDelta(9, ioop.Type)
		fmt.Println("TODO: LZ4DIFF BSDIFF")

	// Like PUFFDIFF, but compressed with lzma4.
	case InstallOperation_LZ4DIFF_PUFFDIFF: //delta >= 9
		iops.checkDelta(9, ioop.Type)
		fmt.Println("TODO: LZ4DIFF PUFFDIFF")

	default:
		fmt.Printf("Unsupported operation type %d (%s), please report a bug!\n", ioop.Type, ioop.Type)
		os.Exit(1)
	}

	//Free the reserved memory
	iops.Payload.InstallSession.free(dataLength)
}
