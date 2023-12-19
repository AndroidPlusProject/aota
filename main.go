package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/dsnet/compress/bzip2"
	"github.com/golang/protobuf/proto"
	"github.com/mholt/archiver/v4"
	crunch "github.com/superwhiskers/crunch/v3"
	humanize "github.com/dustin/go-humanize"
	xz "github.com/spencercw/go-xz"
)

const (
	payloadFilename = "payload.bin"
	payloadMagic = "CrAU"
	brilloMajorPayloadVersion = 2
)

var (
	dataLen  = uint64(0)
	dataCap  = uint64(0)
	dataLock sync.Mutex
)

func main() {
	startTime := time.Now()
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %s <input> [(optional) file to extract...]\n", os.Args[0])
		os.Exit(1)
	}

	runtime.GOMAXPROCS(runtime.NumCPU())
	dataCap = uint64(runtime.NumCPU() * 4096000)
	fmt.Printf("Up to %dMiB of memory may be used to buffer operations on all cores\n", (runtime.NumCPU() * 4))

	var buf *crunch.Buffer
	filename := os.Args[1]
	extractFiles := os.Args[2:]

	zr, err := zip.OpenReader(filename)
	if err == nil {
		zr.Close()
		fsys, err := archiver.FileSystem(context.Background(), filename)
		if err != nil {
			fmt.Printf("Error opening archive: %v\n", err)
			os.Exit(1)
		} else {
			fmt.Printf("Input is an archive, reading %s into memory...\n", payloadFilename)
			bin, err := fsys.Open(payloadFilename)
			if err != nil {
				fmt.Printf("Error opening payload from archive: %v\n", err)
				os.Exit(1)
			}
			data, err := io.ReadAll(bin)
			if err != nil {
				fmt.Printf("Error reading payload from archive: %v\n", err)
				os.Exit(1)
			}
			buf = crunch.NewBuffer(data)
		}
	} else {
		fmt.Printf("Reading %s into memory...\n", filename)
		bin, err := os.Open(filename)
		if err != nil {
			fmt.Printf("Error opening payload: %v\n", err)
			os.Exit(1)
		}
		data, err := io.ReadAll(bin)
		if err != nil {
			fmt.Printf("Error reading payload: %v\n", err)
			os.Exit(1)
		}
		buf = crunch.NewBuffer(data)
	}

	fmt.Println("Parsing payload...")
	blockSize, partitions, baseOffset, err := parsePayload(buf)
	if err != nil {
		fmt.Printf("Error parsing payload: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("- %d partitions (offset 0x%X, block size %d)\n", len(partitions), baseOffset, blockSize)

	iops := NewInstallOps(buf, blockSize, baseOffset, partitions, extractFiles)
	for i := 0; i < len(iops.Map); i++ {
		iop := iops.Map[i]
		fmt.Printf("> %s = %s (%d ops)\n", iop.Name, humanize.Bytes(iop.Size), iop.Ops)
	}
	fmt.Printf("= %d install operations remaining\n", len(iops.Ops))
	iops.Run()

	deltaTime := time.Now().Sub(startTime)
	minutes := int(math.Floor(deltaTime.Minutes()))
	seconds := int(math.Floor(deltaTime.Seconds()))
	if minutes > 0 {
		for seconds > 59 {
			seconds -= 60
		}
	}
	pluralM := "minutes"
	pluralS := "seconds"
	if minutes == 1 {
		pluralM = "minute"
	}
	if seconds == 1 {
		pluralS = "second"
	}
	fmt.Printf("Operation finished in %d %s and %d %s\n", minutes, pluralM, seconds, pluralS)
}

func parsePayload(buf *crunch.Buffer) (uint64, []*PartitionUpdate, uint64, error) {
	// magic
	magic := buf.ReadBytesNext(int64(len(payloadMagic)))
	if string(magic) != payloadMagic {
		return 0, nil, 0, fmt.Errorf("Incorrect magic %s", magic)
	}
	// version & lengths
	version := buf.ReadU64BENext(1)[0]
	if version != brilloMajorPayloadVersion {
		return 0, nil, 0, fmt.Errorf("Unsupported payload version %d, requires %d",	version, brilloMajorPayloadVersion)
	}
	manifestLen := buf.ReadU64BENext(1)[0]
	if !(manifestLen > 0) {
		return 0, nil, 0, fmt.Errorf("Incorrect manifest length %d", manifestLen)
	}
	metadataSigLen := buf.ReadU32BENext(1)[0]
	if !(metadataSigLen > 0) {
		return 0, nil, 0, fmt.Errorf("Incorrect metadata signature length %d", metadataSigLen)
	}
	// manifest
	manifestRaw := buf.ReadBytesNext(int64(manifestLen))
	if uint64(len(manifestRaw)) != manifestLen {
		return 0, nil, 0, fmt.Errorf("Failed to read a manifest with %d bytes", manifestLen)
	}
	manifest := &DeltaArchiveManifest{}
	if err := proto.Unmarshal(manifestRaw, manifest); err != nil {
		return 0, nil, 0, err
	}
	// only support full payloads!
	if *manifest.MinorVersion != 0 {
		return 0, nil, 0, fmt.Errorf("Delta payloads not implemented")
	}
	// copy the partitions from the manifest
	blockSize := uint64(*manifest.BlockSize)
	partitions := make([]*PartitionUpdate, len(manifest.Partitions))
	for i := 0; i < len(partitions); i++ {
		partition := *manifest.Partitions[i]
		partitions[i] = &partition
	}
	// garbage collect the manifest
	manifest = nil
	return blockSize, partitions, 24 + manifestLen + uint64(metadataSigLen), nil
}

type InstallOpsPartition struct {
	sync.Mutex //Locks writes to the extracted file

	Name string
	Ops  int
	Size uint64
	File *os.File
}

type InstallOpsStats struct {
	sync.Mutex //Locks changes to stats to prevent stat confusion

	Threads sync.WaitGroup //Threads remaining
	Counter int            //Next install op to be worked
}

type InstallOps struct {
	Map []*InstallOpsPartition
	Ops []*InstallOperation
	Order []int //Shuffled index mapping of ops
	Stats *InstallOpsStats

	Payload    *crunch.Buffer
	BlockSize  uint64
	BaseOffset uint64
}

func NewInstallOps(payload *crunch.Buffer, blockSize, baseOffset uint64, partitions []*PartitionUpdate, extract []string) *InstallOps {
	iops := &InstallOps{
		Map: make([]*InstallOpsPartition, 0),
		Ops: make([]*InstallOperation, 0),
		Order: make([]int, 0),
		Stats: &InstallOpsStats{},
		Payload: payload,
		BlockSize: blockSize,
		BaseOffset: baseOffset,
	}

	for i := 0; i < len(partitions); i++ {
		p := partitions[i]
		if p.PartitionName == nil {
			continue
		}
		if extract != nil && len(extract) > 0 {
			if !contains(extract, *p.PartitionName) {
				continue
			}
		}

		iop := &InstallOpsPartition{
			Name: *p.PartitionName,
			Ops: len(p.Operations),
		}
		for j := 0; j < len(p.Operations); j++ {
			iop.Size += *p.Operations[j].DataLength
		}

		out := fmt.Sprintf("%s.img", iop.Name)
		_ = os.Remove(out)
		file, err := os.Create(out)
		if err != nil {
			fmt.Printf("Failed to create %s: %v\n", out, err)
			os.Exit(1)
		}
		iop.File = file

		iops.Map = append(iops.Map, iop)
		iops.Ops = append(iops.Ops, p.Operations...)
	}

	//Shuffle the ordered list so we don't just multithread a single partition
	for j := 0; j < len(iops.Ops); j++ {
		if len(iops.Order) > 0 {
			add := iops.Order[len(iops.Order)-1] + 1
			iops.Order = append(iops.Order, add)
		} else {
			iops.Order = append(iops.Order, 0)
		}
	}
	rand.Seed(time.Now().UnixNano())
	rand.Shuffle(len(iops.Order), func(i, j int) {iops.Order[i], iops.Order[j] = iops.Order[j], iops.Order[i]})

	return iops
}

func (iops *InstallOps) Run() {
	if len(iops.Map) == 0 || len(iops.Ops) == 0 {
		return
	}
	for i := 0; i < len(iops.Order); i++ {
		iops.Stats.Threads.Add(1)
		go func(op int) {
			defer iops.Stats.Threads.Done()
			iops.writeOp(op)
		}(i)
	}
	iops.Stats.Threads.Wait()
}

func (iops *InstallOps) getPart(op int) *InstallOpsPartition {
	if len(iops.Map) == 0 || len(iops.Ops) == 0 || op >= len(iops.Ops) {
		return nil
	}

	index := 0
	for i := 0; i < len(iops.Map); i++ {
		if op < (index + iops.Map[i].Ops) {
			return iops.Map[i]
		}
		index += iops.Map[i].Ops
	}

	return nil
}

func (iops *InstallOps) writeOp(opIndex int) {
	op := iops.Order[opIndex]
	part := iops.getPart(op)
	if part == nil {
		fmt.Printf("Nil part when attempting to write op %d\n", op)
		os.Exit(1)
	}
	ioop := iops.Ops[op]
	seek := int64(iops.BaseOffset + *ioop.DataOffset)
	dataLength := *ioop.DataLength

	//Reserve enough free memory
	if dataLen >= dataCap {
		for {
			time.Sleep(time.Millisecond * 100)
			dataLock.Lock()
			if dataLen < dataCap || dataLen == 0 {
				break
			}
			dataLock.Unlock()
		}
	} else {
		dataLock.Lock()
	}
	dataLen += dataLength
	dataLock.Unlock()

	//Read the bytes for the operation
	data := iops.Payload.ReadBytes(seek, int64(dataLength))

	//Write the bytes for the operation
	part.Lock()
	partSeek := int64(*ioop.DstExtents[0].StartBlock * iops.BlockSize)
	if _, err := part.File.Seek(partSeek, 0); err != nil {
		fmt.Printf("Failed to seek to %d in output for %s op %d: %v\n", partSeek, part.Name, op, err)
		os.Exit(1)
	}

	switch *ioop.Type {
	case InstallOperation_REPLACE:
		if _, err := part.File.Write(data); err != nil {
			fmt.Printf("Failed to write output for %s op %d: %v\n", part.Name, op, err)
			os.Exit(1)
		}
	case InstallOperation_REPLACE_BZ:
		bzr, err := bzip2.NewReader(bytes.NewReader(data), nil)
		if err != nil {
			fmt.Printf("Failed to init bzip2 for %s op %d: %v\n", part.Name, op, err)
			os.Exit(1)
		}
		if _, err := io.Copy(part.File, bzr); err != nil {
			fmt.Printf("Failed to write bzip2 output for %s op %d: %v\n", part.Name, op, err)
			os.Exit(1)
		}
	case InstallOperation_REPLACE_XZ:
		xzr := xz.NewDecompressionReader(bytes.NewReader(data))
		if _, err := io.Copy(part.File, &xzr); err != nil {
			fmt.Printf("Failed to write xz output for %s op %d: %v\n", part.Name, op, err)
			os.Exit(1)
		}
	case InstallOperation_ZERO:
		for _, ext := range ioop.DstExtents {
			partSeek = int64(*ext.StartBlock * iops.BlockSize)
			if _, err := part.File.Seek(partSeek, 0); err != nil {
				fmt.Printf("Failed to seek to %d in output for %s op %d: %v\n", partSeek, part.Name, op, err)
				os.Exit(1)
			}
			if _, err := io.Copy(part.File, bytes.NewReader(make([]byte, *ext.NumBlocks*iops.BlockSize))); err != nil {
				fmt.Printf("Failed to write zero output for %s op %d: %v\n", part.Name, op, err)
				os.Exit(1)
			}
		}
	default:
		fmt.Printf("Unsupported operation type %d (%s), please report a bug!\n", *ioop.Type, InstallOperation_Type_name[int32(*ioop.Type)])
		os.Exit(1)
	}
	part.Unlock()

	dataLock.Lock()
	dataLen -= *ioop.DataLength
	dataLock.Unlock()
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
