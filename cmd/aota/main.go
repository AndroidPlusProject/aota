package main

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"time"

	"github.com/AndroidPlusProject/aota"
	"github.com/spf13/pflag"
	humanize "github.com/dustin/go-humanize"
)

//Command line arguments for this session
var (
	dataCap = uint64(65536000)
	debug   = false
	in      = make([]string, 0)
	out     = "."
	extract = make([]string, 0)
	jobs    = 0
)

func init() {
	jobs = runtime.NumCPU()
}

func main() {
	startTime := time.Now()

	wd, err := os.Getwd()
	if err != nil {
		fmt.Printf("Error getting current directory: %v\n", err)
		os.Exit(1)
	}
	out = wd

	pflag.StringArrayVarP(&in,      "in",      "i", in,      "path to payload.bin or OTA.zip (specify again to simulate multiple installs)")
	pflag.StringVarP(     &out,     "out",     "o", out,     "directory for image writing or patching")
	pflag.StringSliceVarP(&extract, "extract", "e", extract, "comma-separated partition list to write (default all, works with multiple payloads)")
	pflag.Uint64VarP(     &dataCap, "cap",     "c", dataCap, "byte ceiling for buffering install operations")
	pflag.BoolVarP(       &debug,   "debug",   "d", debug,   "debug mode, everything is log spam")
	pflag.IntVarP(        &jobs,    "jobs",    "j", jobs,    "limit for how many Go processes can spawn")
	pflag.Parse()

	runtime.GOMAXPROCS(jobs)

	fmt.Println("Initializing install session...")
	taskStart := time.Now()
	is, err := aota.NewInstallSessionMulti(in, extract, out)
	if err != nil {
		fmt.Printf("Error initializing installer: %v\n", err)
		os.Exit(1)
	}
	is.SetMemoryLimit(dataCap)
	is.SetDebug(debug)
	if debug { fmt.Printf("Task time: %dms\n", time.Now().Sub(taskStart).Milliseconds()) }

	for i := 0; i < len(is.Payloads); i++ {
		p := is.Payloads[i]
		fmt.Printf("- %s\n  Partitions: %d\n  Offset: 0x%X\n  Block Size: %d\n", p.In, len(p.Partitions), p.BaseOffset, p.BlockSize)
		for j := 0; j < len(p.Installer.Map); j++ {
			iop := p.Installer.Map[j]
			fmt.Printf("> %s = %s (%d ops)\n", iop.Name, humanize.Bytes(iop.Size), iop.Ops)
		}
		fmt.Printf("= %d total install operations\n", len(p.Installer.Ops))
	}

	fmt.Println("Installing...")
	taskStart = time.Now()
	is.Install()
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
