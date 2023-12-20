package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dsnet/compress/bzip2"
	"github.com/golang/protobuf/proto"
	"github.com/spf13/pflag"
	crunch "github.com/superwhiskers/crunch/v3"
	humanize "github.com/dustin/go-humanize"
	xz "github.com/jamespfennell/xz"
)

const (
	payloadFilename = "payload.bin"
	payloadMagic = "CrAU"
	brilloMajorPayloadVersion = 2
)

//Tracking for memory usage
var (
	dataLen  = 0
	dataLock sync.Mutex
	tmpPayloads = make([]string, 0)
)

//Command line arguments for this session
var (
	dataCap = 0
	debug   = false
	//preload = false
	in      = make([]string, 0)
	out     = "."
	extract = make([]string, 0)
)

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
}

func main() {
	startTime := time.Now()

	pflag.IntVarP(        &dataCap, "cap",     "c", runtime.NumCPU() * 4096000, "byte ceiling for buffering install operations")
	pflag.BoolVarP(       &debug,   "debug",   "d", debug,                      "debug mode, everything is log spam")
	//pflag.BoolVarP(       &preload, "preload", "p", preload,                    "preloads the payload into memory first")
	pflag.StringArrayVarP(&in,      "in",      "i", in,                         "path to payload.bin or OTA.zip")
	pflag.StringVarP(     &out,     "out",     "o", out,                        "directory for image writing or patching")
	pflag.StringSliceVarP(&extract, "extract", "e", extract,                    "comma-separated partition list to write")
	pflag.Parse()

	fmt.Println("Initializing payload session...")
	taskStart := time.Now()
	ps, err := NewPayloadSession(in, extract, out)
	if err != nil {
		fmt.Printf("Error opening payload: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if len(tmpPayloads) > 0 {
			for _, tmpPayload := range tmpPayloads {
				if err := os.Remove(tmpPayload); err != nil {
					fmt.Printf("Unable to delete %s: %v\n", tmpPayload, err)
				}
			}
		}
	}()
	if debug { fmt.Printf("Task time: %dms\n", time.Now().Sub(taskStart).Milliseconds()) }

	for i := 0; i < len(ps.Payloads); i++ {
		p := ps.Payloads[i]
		fmt.Printf("- %s\n  Partitions: %d\n  Offset: 0x%X\n  Block Size: %d\n", p.In, len(p.Partitions), p.BaseOffset, p.BlockSize)
		for j := 0; j < len(p.Installer.Map); j++ {
			iop := p.Installer.Map[j]
			fmt.Printf("> %s = %s (%d ops)\n", iop.Name, humanize.Bytes(iop.Size), iop.Ops)
		}
		fmt.Printf("= %d total install operations\n", len(p.Installer.Ops))
	}

	fmt.Println("Installing...")
	taskStart = time.Now()
	ps.Run()
	if debug { fmt.Printf("Task time: %dms\n", time.Now().Sub(taskStart).Milliseconds()) }

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

type PayloadSession struct {
	Payloads []*Payload
}

func NewPayloadSession(input, extract []string, outDir string) (*PayloadSession, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("must provide at least one payload as input")
	}

	ps := &PayloadSession{
		Payloads: make([]*Payload, 0),
	}

	for i := 0; i < len(input); i++ {
		n, err := ps.AddPayload(input[i], extract)
		if err != nil {
			return nil, fmt.Errorf("error adding payload (%s): %v", input[i], err)
		}

		iops, err := NewInstallOps(ps.Payloads[n], outDir)
		if err != nil {
			return nil, fmt.Errorf("error initializing payload (%s): %v", input[i], err)
		}
		ps.Payloads[n].Installer = iops
	}

	return ps, nil
}

func (ps *PayloadSession) AddPayload(in string, extract []string) (n int, err error) {
	n = -1

	var payload *Payload
	if strings.HasPrefix(in, "http://") || strings.HasPrefix(in, "https://") {
		payload, err = NewPayloadURL(in, extract)
		if err != nil {
			return
		}
	} else {
		payload, err = NewPayloadFile(in, extract)
		if err != nil {
			return
		}
	}
	ps.Payloads = append(ps.Payloads, payload)

	n = len(ps.Payloads)-1
	return
}

func (ps *PayloadSession) Run() {
	for i := 0; i < len(ps.Payloads); i++ {
		ps.Payloads[i].Installer.Run()
	}
}

type Payload struct {
	sync.Mutex //Locks attempts to seek or read the consumer

	In        string
	Extract   []string
	Consumer  io.ReadSeeker

	BaseOffset uint64
	BlockSize  uint64
	Partitions []*PartitionUpdate

	Session   *PayloadSession
	Installer *InstallOps
}

func NewPayloadURL(in string, extract []string) (*Payload, error) {
	return nil, fmt.Errorf("network payloads are not supported yet")
}

func NewPayloadFile(in string, extract []string) (*Payload, error) {
	payload := &Payload{
		In: in,
		Extract: extract,
	}

	bin, err := os.Open(in)
	if err != nil {
		return nil, fmt.Errorf("error opening payload: %v", err)
	}
	stat, err := bin.Stat()
	if err != nil {
		return nil, fmt.Errorf("error getting stat on payload: %v", err)
	}
	if zrc, err := zip.NewReader(bin, stat.Size()); err == nil {
		zrcPayload, err := zrc.Open(payloadFilename)
		if err != nil {
			return nil, fmt.Errorf("error finding payload inside archive: %v", err)
		}
		tmpPayload, err := os.CreateTemp("", "aota_payload-*.bin")
		if err != nil {
			return nil, fmt.Errorf("error creating temp payload: %v", err)
		}
		tmpPayloads = append(tmpPayloads, tmpPayload.Name())
		if _, err := io.Copy(tmpPayload, zrcPayload); err != nil {
			return nil, fmt.Errorf("error extracting temp payload: %v", err)
		}
		bin = tmpPayload
	}
	bin.Seek(0, 0) //Reset the seek position
	payload.Consumer = bin
	return payload, payload.Parse()
}

func (payload *Payload) Parse() error {
	if payload.Consumer == nil {
		return fmt.Errorf("cannot parse nil consumer")
	}

	headerSize := uint64(len(payloadMagic) + 8 + 8 + 4) //magic + version(u64) + manifestLen(u64) + metadataSignatureLen(u32)
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(payload.Consumer, header); err != nil {
		return fmt.Errorf("error reading payload header: %v", err)
	}
	buf := crunch.NewBuffer(header)

	magic := buf.ReadBytesNext(int64(len(payloadMagic)))
	if string(magic) != payloadMagic {
		return fmt.Errorf("incorrect payload magic: %s", magic)
	}
	version := buf.ReadU64BENext(1)[0]
	if version != brilloMajorPayloadVersion {
		return fmt.Errorf("unsupported payload version %d, requires %d", version, brilloMajorPayloadVersion)
	}
	manifestLen := buf.ReadU64BENext(1)[0]
	if manifestLen == 0 {
		return fmt.Errorf("manifest cannot be empty")
	}
	metadataSigLen := buf.ReadU32BENext(1)[0]
	if metadataSigLen == 0 {
		return fmt.Errorf("metadata signature cannot be empty")
	}

	manifestRaw := make([]byte, manifestLen)
	if _, err := io.ReadFull(payload.Consumer, manifestRaw); err != nil {
		return fmt.Errorf("error reading payload manifest: %v", err)
	}
	manifest := &DeltaArchiveManifest{}
	if err := proto.Unmarshal(manifestRaw, manifest); err != nil {
		return fmt.Errorf("error parsing payload protobuf: %v", err)
	}
	if *manifest.MinorVersion != 0 {
		return fmt.Errorf("incremental payloads are not supported yet")
	}

	payload.BaseOffset = headerSize + manifestLen + uint64(metadataSigLen)
	payload.BlockSize = uint64(*manifest.BlockSize)
	payload.Partitions = make([]*PartitionUpdate, 0)
	for i := 0; i < len(manifest.Partitions); i++ {
		if payload.Extract != nil && len(payload.Extract) > 0 {
			if !contains(payload.Extract, *manifest.Partitions[i].PartitionName) {
				continue
			}
		}
		payload.Partitions = append(payload.Partitions, manifest.Partitions[i])
	}

	return nil
}

func (payload *Payload) ReadBytes(offset, length int64) ([]byte, error) {
	payload.Lock()
	defer payload.Unlock()
	if _, err := payload.Consumer.Seek(offset, 0); err != nil {
		return nil, err
	}
	data := make([]byte, length)
	if n, err := payload.Consumer.Read(data); err != nil {
		return nil, fmt.Errorf("read %d bytes: %v", n, err)
	}
	return data, nil
}

type InstallOpsPartition struct {
	sync.Mutex //Locks writes to the extracted file

	Name string
	Ops  int
	Size uint64
	File *os.File
}

type InstallOpsSession struct {
	sync.Mutex //Locks changes to stats to prevent stat confusion

	Threads sync.WaitGroup //Threads remaining
}

type InstallOpStep struct {
	Op int
	Iop *InstallOpsPartition
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
			fmt.Printf("Failed to create %s: %v\n", out, err)
			os.Exit(1)
		}
		iop.File = file

		iops.Map = append(iops.Map, iop)
		iops.Ops = append(iops.Ops, p.Operations...)

		if debug { fmt.Printf("Task time: %dms\n", time.Now().Sub(taskStart).Milliseconds()) }
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
	dataLen += int(dataLength)
	dataLock.Unlock()

	//Read the bytes for the operation
	data, err := iops.Payload.ReadBytes(seek, int64(dataLength))
	if err != nil {
		fmt.Printf("Failed to read %d bytes from offset %d in payload for %s op %d: %v\n", dataLength, seek, iop.Name, op, err)
		os.Exit(1)
	}

	//Write the bytes for the operation
	iop.Lock()
	partSeek := int64(*ioop.DstExtents[0].StartBlock * iops.BlockSize)
	if _, err := iop.File.Seek(partSeek, 0); err != nil {
		fmt.Printf("Failed to seek to %d in output for %s op %d: %v\n", partSeek, iop.Name, op, err)
		os.Exit(1)
	}

	switch *ioop.Type {
	case InstallOperation_REPLACE:
		if _, err := iop.File.Write(data); err != nil {
			fmt.Printf("Failed to write output for %s op %d: %v\n", iop.Name, op, err)
			os.Exit(1)
		}
	case InstallOperation_REPLACE_BZ:
		bzr, err := bzip2.NewReader(bytes.NewReader(data), nil)
		if err != nil {
			fmt.Printf("Failed to init bzip2 for %s op %d: %v\n", iop.Name, op, err)
			os.Exit(1)
		}
		if _, err := io.Copy(iop.File, bzr); err != nil {
			fmt.Printf("Failed to write bzip2 output for %s op %d: %v\n", iop.Name, op, err)
			os.Exit(1)
		}
	case InstallOperation_REPLACE_XZ:
		xzr := xz.NewReader(bytes.NewReader(data))
		if _, err := io.Copy(iop.File, xzr); err != nil {
			fmt.Printf("Failed to write xz output for %s op %d: %v\n", iop.Name, op, err)
			os.Exit(1)
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

	dataLock.Lock()
	dataLen -= int(*ioop.DataLength)
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
