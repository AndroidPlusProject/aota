package aota

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/golang/protobuf/proto"
	crunch "github.com/superwhiskers/crunch/v3"
	humanize "github.com/dustin/go-humanize"
)

const (
	brilloMajorPayloadVersion = 2
	payloadFilename           = "payload.bin"
	payloadMagic              = "CrAU"
)

type Payload struct {
	sync.Mutex //Locks attempts to seek or read the consumer

	Temporary bool
	In        string
	Extract   []string
	Consumer  *PayloadConsumer
	Manifest  *DeltaArchiveManifest

	BaseOffset uint64
	BlockSize  uint64
	Partitions []*PartitionUpdate

	Installer      *InstallOps
	InstallSession *InstallSession
}

func (payload *Payload) String() string {
	parts := make([]string, 0)
	size := uint64(0)
	ops := 0
	for i := 0; i < len(payload.Partitions); i++ {
		part := payload.Partitions[i]
		pSize := uint64(0)
		for j := 0; j < len(part.Operations); j++ {
			pSize += *part.Operations[j].DataLength
		}
		partStr := fmt.Sprintf("- %s: %s (%d ops)", *part.PartitionName, humanize.Bytes(pSize), len(part.Operations))

		parts = append(parts, partStr)
		size += pSize
		ops += len(part.Operations)
	}
	return fmt.Sprintf("in:'%s' extract:%v partitions:%d size:'%s' ops:%d offset:%d block:%d\n%s",
		payload.In, payload.Extract, len(payload.Partitions), humanize.Bytes(size),
		ops, payload.BaseOffset, payload.BlockSize, strings.Join(parts, "\n"))
}

func NewPayloadURL(in string, extract []string) (*Payload, error) {
	return nil, fmt.Errorf("network payloads are not supported yet")
}

func NewPayloadFile(in string, extract []string, extractArchive bool) (*Payload, error) {
	payload := &Payload{
		In: in,
		Extract: extract,
		Consumer: &PayloadConsumer{},
		Manifest: &DeltaArchiveManifest{},
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
		if extractArchive {
			tmpPayload, err := os.CreateTemp("", "aota_payload-*.bin")
			if err != nil {
				return nil, fmt.Errorf("error creating temp payload: %v", err)
			}
			tmpPayloadName := tmpPayload.Name()
			if _, err := io.Copy(tmpPayload, zrcPayload); err != nil {
				zrcPayload.Close()
				tmpPayload.Close()
				os.Remove(tmpPayloadName)
				return nil, fmt.Errorf("error extracting temp payload: %v", err)
			}
			tmpPayload.Seek(0, 0) //Reset the position since we wrote to it

			//Re-open the archived payload to reset its read position
			zrcPayload.Close()
			zrcPayload, err = zrc.Open(payloadFilename)
			if err != nil {
				tmpPayload.Close()
				os.Remove(tmpPayloadName)
				return nil, fmt.Errorf("error re-opening payload inside archive: %v", err)
			}

			payload.Consumer.Seekable = tmpPayload
			payload.In = tmpPayloadName
			payload.Temporary = true
		}
		payload.Consumer.Readable = zrcPayload
	} else {
		payload.Consumer.Seekable = bin
	}
	return payload, payload.Parse()
}

func (payload *Payload) Close() error {
	//TODO: Fight for .Installer locks to set .Closed=true for graceful worker deaths
	//Wait for all workers to finish before returning from Close()

	//Close out active handles
	payload.Consumer.Close()
	if payload.Temporary {
		if err := os.Remove(payload.In); err != nil {
			return err
		}
	}

	//Dereference everything
	payload.Partitions = nil
	payload.Installer = nil

	return nil
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
	if err := proto.Unmarshal(manifestRaw, payload.Manifest); err != nil {
		return fmt.Errorf("error parsing payload protobuf: %v", err)
	}
	if *payload.Manifest.MinorVersion != 0 {
		return fmt.Errorf("incremental payloads are not supported yet")
	}

	payload.BaseOffset = headerSize + manifestLen + uint64(metadataSigLen)
	payload.BlockSize = uint64(*payload.Manifest.BlockSize)
	payload.Partitions = make([]*PartitionUpdate, 0)
	for i := 0; i < len(payload.Manifest.Partitions); i++ {
		if payload.Extract != nil && len(payload.Extract) > 0 {
			if !contains(payload.Extract, *payload.Manifest.Partitions[i].PartitionName) {
				continue
			}
		}
		payload.Partitions = append(payload.Partitions, payload.Manifest.Partitions[i])
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
