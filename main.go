package main

import (
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/mholt/archiver/v4"
	"github.com/xi2/xz"
)

const (
	payloadFilename = "payload.bin"

	zipMagic     = "PK"
	payloadMagic = "CrAU"

	brilloMajorPayloadVersion = 2
)

var (
	activeFiles = 0
	dataLen     = uint64(0)
	dataCap     = uint64(0)
	lock        sync.Mutex
)

func main() {
	startTime := time.Now()
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %s <input> [(optional) file to extract...]\n", os.Args[0])
		os.Exit(1)
	}

	var f io.ReadSeeker
	filename := os.Args[1]
	extractFiles := os.Args[2:]

	zr, err := zip.OpenReader(filename)
	if err == nil {
		zr.Close()
		fsys, err := archiver.FileSystem(context.Background(), filename)
		if err != nil {
			log.Printf("Error opening archive: %v", err)
			os.Exit(1)
		} else {
			log.Printf("Input is an archive, reading %s into memory...", payloadFilename)
			bin, err := fsys.Open(payloadFilename)
			if err != nil {
				log.Printf("Error opening payload from archive: %v", err)
				os.Exit(1)
			}
			data, err := io.ReadAll(bin)
			if err != nil {
				log.Printf("Error reading payload from archive: %v", err)
				os.Exit(1)
			}
			f = bytes.NewReader(data)
		}
	} else {
		log.Printf("Reading %s into memory...", filename)
		bin, err := os.Open(filename)
		if err != nil {
			log.Printf("Error opening payload: %v", err)
			os.Exit(1)
		}
		data, err := io.ReadAll(bin)
		if err != nil {
			log.Printf("Error reading payload: %v", err)
			os.Exit(1)
		}
		f = bytes.NewReader(data)
	}

	blockSize, partitions, baseOffset, err := parsePayload(f)
	if err != nil {
		log.Printf("Error parsing payload: %v", err)
		os.Exit(1)
	}
	dataCap = uint64(runtime.NumCPU() * 4096000)
	log.Printf("Memory cap set to %d bytes for buffering partition images", dataCap)
	if dataCap > 4096000 {
		dataCap -= 4096000 //Potential to overflow <= 4MB
	}
	iops := NewInstallOps(f, blockSize, baseOffset, partitions, extractFiles)
	iops.Run()

	log.Println("Done!")
	deltaTime := time.Now().Sub(startTime)
	minutes := int(math.Floor(deltaTime.Minutes()))
	seconds := int(math.Floor(deltaTime.Seconds()))
	if minutes > 0 {
		for seconds > 59 {
			seconds -= 60
		}
	}
	log.Printf("Operation took %d minute(s) and %d second(s)", minutes, seconds)
}

func parsePayload(r io.ReadSeeker) (uint64, []*PartitionUpdate, uint64, error) {
	log.Println("Parsing payload...")
	// magic
	magic := make([]byte, len(payloadMagic))
	_, err := r.Read(magic)
	if err != nil || string(magic) != payloadMagic {
		return 0, nil, 0, fmt.Errorf("Incorrect magic %s", magic)
	}
	// version & lengths
	var version, manifestLen uint64
	var metadataSigLen uint32
	err = binary.Read(r, binary.BigEndian, &version)
	if err != nil || version != brilloMajorPayloadVersion {
		return 0, nil, 0, fmt.Errorf("Unsupported payload version %d, requires %d",	version, brilloMajorPayloadVersion)
	}
	err = binary.Read(r, binary.BigEndian, &manifestLen)
	if err != nil || !(manifestLen > 0) {
		return 0, nil, 0, fmt.Errorf("Incorrect manifest length %d", manifestLen)
	}
	err = binary.Read(r, binary.BigEndian, &metadataSigLen)
	if err != nil || !(metadataSigLen > 0) {
		return 0, nil, 0, fmt.Errorf("Incorrect metadata signature length %d", metadataSigLen)
	}
	// manifest
	manifestRaw := make([]byte, manifestLen)
	n, err := r.Read(manifestRaw)
	if err != nil || uint64(n) != manifestLen {
		return 0, nil, 0, fmt.Errorf("Failed to read a manifest with %d bytes", manifestLen)
	}
	manifest := &DeltaArchiveManifest{}
	err = proto.Unmarshal(manifestRaw, manifest)
	if err != nil {
		return 0, nil, 0, err
	}
	// only support full payloads!
	if *manifest.MinorVersion != 0 {
		return 0, nil, 0, fmt.Errorf("Delta payloads not implemented")
	}
	// print manifest info
	log.Printf("Block size: %d, Partition count: %d\n",
		*manifest.BlockSize, len(manifest.Partitions))
	// extract partitions
	blockSize := uint64(*manifest.BlockSize)
	partitions := make([]*PartitionUpdate, len(manifest.Partitions))
	for i := 0; i < len(partitions); i++ {
		partition := *manifest.Partitions[i]
		partitions[i] = &partition
	}
	manifest = nil //Please garbage collect it, please
	return blockSize, partitions, 24 + manifestLen + uint64(metadataSigLen), nil
}

type InstallOpsPartition struct {
	sync.Mutex //Locks writes to extracted file

	Name string
	Ops  int
	Size uint64
	File *os.File
}

type InstallOps struct {
	sync.Mutex //Locks payload reading and seeking

	Map []*InstallOpsPartition
	Ops []*InstallOperation
	Active int

	Payload    io.ReadSeeker
	BlockSize  uint64
	BaseOffset uint64
}

func NewInstallOps(payload io.ReadSeeker, blockSize, baseOffset uint64, partitions []*PartitionUpdate, extract []string) *InstallOps {
	iops := &InstallOps{
		Map: make([]*InstallOpsPartition, 0),
		Ops: make([]*InstallOperation, 0),
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
			log.Fatalf("Failed to create %s: %v", out, err)
		}
		iop.File = file

		iops.Map = append(iops.Map, iop)
		iops.Ops = append(iops.Ops, p.Operations...)
	}

	return iops
}

func (iops *InstallOps) Run() {
	if len(iops.Map) == 0 || len(iops.Ops) == 0 {
		return
	}

	threads := runtime.NumCPU()
	iops.Active = threads
	ops := int(math.Ceil(float64(len(iops.Ops)) / float64(threads)))

	for i := 0; i < threads; i++ {
		go iops.runThread(i*ops, ops)
	}

	for iops.Active > 0 {
		time.Sleep(time.Millisecond * 100)
	}
}

func (iops *InstallOps) runThread(index, ops int) {
	if index >= len(iops.Ops) {
		return
	}
	if (index+ops) >= len(iops.Ops) {
		ops = len(iops.Ops)-index
	}

	for i := index; i < (index+ops); i++ {
		iops.writeOp(i)
	}

	iops.Lock()
	iops.Active--
	iops.Unlock()
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

func (iops *InstallOps) writeOp(op int) {
	part := iops.getPart(op)
	if part == nil {
		log.Fatalf("Nil part when attempting to write op %d", op)
	}
	ioop := iops.Ops[op]
	seek := int64(iops.BaseOffset + *ioop.DataOffset)
	dataLength := *ioop.DataLength

	//Reserve enough free memory
	if dataLen >= dataCap {
		for {
			time.Sleep(time.Millisecond * 100)
			iops.Lock()
			if dataLen < dataCap || dataLen == 0 {
				break
			}
			iops.Unlock()
		}
	} else {
		iops.Lock()
	}
	dataLen += dataLength

	//Read the bytes for the operation
	_, err := iops.Payload.Seek(seek, 0)
	if err != nil {
		log.Fatalf("Op %d failed to seek to %d: %v", op, seek, err)
	}
	data := make([]byte, dataLength)
	n, err := iops.Payload.Read(data)
	if err != nil {
		log.Fatalf("Op %d failed to read %d bytes: %v", op, dataLength, err)
	}
	if uint64(n) != dataLength {
		log.Fatalf("Op %d expected %d bytes, got %d bytes", op, dataLength, n)
	}
	iops.Unlock()

	//Write the bytes for the operation
	part.Lock()
	partSeek := int64(*ioop.DstExtents[0].StartBlock * iops.BlockSize)
	_, err = part.File.Seek(partSeek, 0)
	if err != nil {
		log.Fatalf("Failed to seek to %d in output for %s op %d: %v", partSeek, part.Name, op, err)
	}

	switch *ioop.Type {
	case InstallOperation_REPLACE:
		_, err = part.File.Write(data)
		if err != nil {
			log.Fatalf("Failed to write output for %s op %d: %v", part.Name, op, err)
		}
	case InstallOperation_REPLACE_BZ:
		bzr := bzip2.NewReader(bytes.NewReader(data))
		_, err = io.Copy(part.File, bzr)
		if err != nil {
			log.Fatalf("Failed to write bzip2 output for %s op %d: %v", part.Name, op, err)
		}
	case InstallOperation_REPLACE_XZ:
		xzr, err := xz.NewReader(bytes.NewReader(data), 0)
		if err != nil {
			log.Fatalf("Bad xz data in %s: %v", part.Name, err)
		}
		_, err = io.Copy(part.File, xzr)
		if err != nil {
			log.Fatalf("Failed to write xz output for %s op %d: %v", part.Name, op, err)
		}
	case InstallOperation_ZERO:
		for _, ext := range ioop.DstExtents {
			partSeek = int64(*ext.StartBlock * iops.BlockSize)
			_, err = part.File.Seek(partSeek, 0)
			if err != nil {
				log.Fatalf("Failed to seek to %d in output for %s op %d: %v", partSeek, part.Name, op, err)
			}
			_, err = io.Copy(part.File, bytes.NewReader(make([]byte, *ext.NumBlocks*iops.BlockSize)))
			if err != nil {
				log.Fatalf("Failed to write zero output for %s op %d: %v", part.Name, op, err)
			}
		}
	default:
		log.Fatalf("Unsupported operation type %d (%s), please report a bug!", *ioop.Type, InstallOperation_Type_name[int32(*ioop.Type)])
	}
	part.Unlock()

	iops.Lock()
	dataLen -= *ioop.DataLength
	iops.Unlock()
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
