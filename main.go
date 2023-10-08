package main

import (
	"archive/zip"
	"bytes"
	"compress/bzip2"
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
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %s <input> [(optional) file to extract...]\n", os.Args[0])
		os.Exit(1)
	}
	startTime := time.Now()
	filename := os.Args[1]
	extractFiles := os.Args[2:]
	f, err := os.Open(filename)
	if err != nil {
		log.Fatalf("Failed to open file: %s", err)
	}
	if isZip(f) {
		// Extract payload.bin from the zip first
		_ = f.Close()
		log.Printf("Input is a zip file, searching for %s ...\n", payloadFilename)
		zr, err := zip.OpenReader(filename)
		if err != nil {
			log.Fatalf("Failed to open the zip file: %s\n", err.Error())
		}
		zf, err := findPayload(zr)
		if err != nil {
			log.Fatalf("Failed to read from the zip file: %s\n", err.Error())
		}
		if zf == nil {
			log.Fatalf("%s not found in the zip file\n", payloadFilename)
		}
		log.Printf("Extracting %s ...\n", payloadFilename)
		_ = os.Remove(payloadFilename)
		f, err = os.Create(payloadFilename)
		if err != nil {
			log.Fatalf("Failed to create the extraction file: %s\n", err.Error())
		}
		_, err = io.Copy(f, zf)
		if err != nil {
			log.Fatalf("Failed to extract: %s\n", err.Error())
		}
		_ = zf.Close()
		_ = zr.Close()
		_, _ = f.Seek(0, 0)
	}
	blockSize, partitions, baseOffset := parsePayload(f)
	dataCap = uint64(runtime.NumCPU() * 4096000)
	log.Printf("Memory cap set to %d bytes for buffering partition images, may overflow <= 4MB (not indicative of total memory usage)\n", dataCap)
	extractPartitions(blockSize, partitions, f, baseOffset, extractFiles)
	// done
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

func isZip(f *os.File) bool {
	header := make([]byte, len(zipMagic))
	_, err := f.Read(header)
	_, _ = f.Seek(0, 0)
	return err == nil && string(header) == zipMagic
}

func findPayload(zr *zip.ReadCloser) (io.ReadCloser, error) {
	for _, f := range zr.File {
		if f.Name == payloadFilename {
			return f.Open()
		}
	}
	return nil, nil
}

func parsePayload(r io.ReadSeeker) (uint32, []*PartitionUpdate, uint64) {
	log.Println("Parsing payload...")
	// magic
	magic := make([]byte, len(payloadMagic))
	_, err := r.Read(magic)
	if err != nil || string(magic) != payloadMagic {
		log.Fatalf("Incorrect magic (%s)\n", string(magic))
	}
	// version & lengths
	var version, manifestLen uint64
	var metadataSigLen uint32
	err = binary.Read(r, binary.BigEndian, &version)
	if err != nil || version != brilloMajorPayloadVersion {
		log.Fatalf("Unsupported payload version (%d). This tool only supports version %d\n",
			version, brilloMajorPayloadVersion)
	}
	err = binary.Read(r, binary.BigEndian, &manifestLen)
	if err != nil || !(manifestLen > 0) {
		log.Fatalf("Incorrect manifest length (%d)\n", manifestLen)
	}
	err = binary.Read(r, binary.BigEndian, &metadataSigLen)
	if err != nil || !(metadataSigLen > 0) {
		log.Fatalf("Incorrect metadata signature length (%d)\n", metadataSigLen)
	}
	// manifest
	manifestRaw := make([]byte, manifestLen)
	n, err := r.Read(manifestRaw)
	if err != nil || uint64(n) != manifestLen {
		log.Fatalf("Failed to read the manifest (%d)\n", manifestLen)
	}
	manifest := &DeltaArchiveManifest{}
	err = proto.Unmarshal(manifestRaw, manifest)
	if err != nil {
		log.Fatalf("Failed to parse the manifest: %s\n", err.Error())
	}
	// only support full payloads!
	if *manifest.MinorVersion != 0 {
		log.Fatalf("Delta payloads are not supported, please use a full payload file\n")
	}
	// print manifest info
	log.Printf("Block size: %d, Partition count: %d\n",
		*manifest.BlockSize, len(manifest.Partitions))
	// extract partitions
	blockSize := *manifest.BlockSize
	partitions := make([]*PartitionUpdate, len(manifest.Partitions))
	for i := 0; i < len(partitions); i++ {
		partition := *manifest.Partitions[i]
		partitions[i] = &partition
	}
	manifest = nil //Please garbage collect it, please
	return blockSize, partitions, 24 + manifestLen + uint64(metadataSigLen)
}

func extractPartitions(blockSize uint32, partitions []*PartitionUpdate, r io.ReadSeeker, baseOffset uint64, extractFiles []string) {
	for _, p := range partitions {
		if p.PartitionName == nil || (len(extractFiles) > 0 && !contains(extractFiles, *p.PartitionName)) {
			continue
		}
		if activeFiles >= runtime.NumCPU() {
			for {
				time.Sleep(time.Millisecond * 100)
				lock.Lock()
				if activeFiles < runtime.NumCPU() {
					activeFiles++
					lock.Unlock()
					break
				}
				lock.Unlock()
			}
		} else {
			lock.Lock()
			activeFiles++
			lock.Unlock()
		}
		go func(p *PartitionUpdate, r io.ReadSeeker, baseOffset uint64, blockSize uint32) {
			size := uint64(0)
			for i := 0; i < len(p.Operations); i++ {
				size += *p.Operations[i].DataLength
			}
			log.Printf("Extracting %s (%d ops = %d bytes) ...", *p.PartitionName, len(p.Operations), size)
			outFilename := fmt.Sprintf("%s.img", *p.PartitionName)
			_ = os.Remove(outFilename)
			extractPartition(p, outFilename, r, baseOffset, blockSize)
			lock.Lock()
			activeFiles--
			lock.Unlock()
		}(p, r, baseOffset, blockSize)
	}
	for activeFiles > 0 {
		time.Sleep(time.Millisecond * 100) //Wait patiently!
	}
}

func extractPartition(p *PartitionUpdate, outFilename string, r io.ReadSeeker, baseOffset uint64, blockSize uint32) {
	outFile, err := os.Create(outFilename)
	if err != nil {
		log.Fatalf("Failed to create the output file: %s\n", err.Error())
	}
	var writeLock sync.Mutex
	activeOps := len(p.Operations)
	for i, op := range p.Operations {
		dataLength := *op.DataLength
		if dataLen >= dataCap {
			for {
				time.Sleep(time.Millisecond * 100)
				lock.Lock()
				if dataLen < dataCap {
					break
				}
				lock.Unlock()
			}
		} else {
			lock.Lock()
		}
		dataLen += dataLength
		log.Printf("- %s (%d/%d ops - %d bytes - total %d/%d bytes)", *p.PartitionName, i+1, len(p.Operations), dataLength, dataLen, dataCap)
		go func(instop *InstallOperation) {
			dataPos := int64(baseOffset + *instop.DataOffset)
			_, err = r.Seek(dataPos, 0)
			if err != nil {
				_ = outFile.Close()
				log.Fatalf("Failed to seek to %d in partition %s: %s\n", dataPos, outFilename, err.Error())
			}
			data := make([]byte, *instop.DataLength)
			n, err := r.Read(data)
			if err != nil || uint64(n) != *instop.DataLength {
				_ = outFile.Close()
				log.Fatalf("Failed to read enough data from partition %s: %s\n", outFilename, err.Error())
			}
			lock.Unlock()

			writeLock.Lock()
			outSeekPos := int64(*instop.DstExtents[0].StartBlock * uint64(blockSize))
			_, err = outFile.Seek(outSeekPos, 0)
			if err != nil {
				_ = outFile.Close()
				log.Fatalf("Failed to seek to %d in partition %s: %s\n", outSeekPos, outFilename, err.Error())
			}

			switch *instop.Type {
			case InstallOperation_REPLACE:
				_, err = outFile.Write(data)
				if err != nil {
					_ = outFile.Close()
					log.Fatalf("Failed to write output to %s: %s\n", outFilename, err.Error())
				}
			case InstallOperation_REPLACE_BZ:
				bzr := bzip2.NewReader(bytes.NewReader(data))
				_, err = io.Copy(outFile, bzr)
				if err != nil {
					_ = outFile.Close()
					log.Fatalf("Failed to write output to %s: %s\n", outFilename, err.Error())
				}
			case InstallOperation_REPLACE_XZ:
				xzr, err := xz.NewReader(bytes.NewReader(data), 0)
				if err != nil {
					_ = outFile.Close()
					log.Fatalf("Bad xz data in partition %s: %s\n", *p.PartitionName, err.Error())
				}
				_, err = io.Copy(outFile, xzr)
				if err != nil {
					_ = outFile.Close()
					log.Fatalf("Failed to write output to %s: %s\n", outFilename, err.Error())
				}
			case InstallOperation_ZERO:
				for _, ext := range instop.DstExtents {
					outSeekPos = int64(*ext.StartBlock * uint64(blockSize))
					_, err = outFile.Seek(outSeekPos, 0)
					if err != nil {
						_ = outFile.Close()
						log.Fatalf("Failed to seek to %d in partition %s: %s\n", outSeekPos, outFilename, err.Error())
					}
					// write zeros
					_, err = io.Copy(outFile, bytes.NewReader(make([]byte, *ext.NumBlocks*uint64(blockSize))))
					if err != nil {
						_ = outFile.Close()
						log.Fatalf("Failed to write output to %s: %s\n", outFilename, err.Error())
					}
				}
			default:
				_ = outFile.Close()
				log.Fatalf("Unsupported operation type: %d (%s), please report a bug\n",
					*instop.Type, InstallOperation_Type_name[int32(*instop.Type)])
			}
			writeLock.Unlock()

			lock.Lock()
			activeOps--
			dataLen -= *instop.DataLength
			lock.Unlock()
		}(op)
	}
	for activeOps > 0 {
		time.Sleep(time.Millisecond * 100) //Wait patiently!
	}
	_ = outFile.Close()
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
