# aota

A multithreaded Android OTA installer, written in Go.

[Download self-hosted releases from the latest commit](https://files.joshuadoes.com/aota) (stripped using `go build -ldflags="-s -w"`)

## Usage

On Linux, Mac, and pretty much everything else:

`./aota -arg value`

On Windows:

`.\aota.exe -arg value`

```
-input <required: path to OTA.zip or payload.bin>
-out <optional: path to output folder>
-extract <optional: comma,separated,partition,list>
-cap <optional: memory limit in bytes for buffering install operations, anything above 64MiB is pointless>
-debug <optional: no value, just shows debug logs when used>
```

For example, to extract boot, dtbo, and vendor_boot from `raven-ota.zip`:

```
aota -i raven-ota.zip -e boot,dtbo,vendor_boot
```

It can parse your `payload.bin` directly, or it can temporarily extract it from an `OTA.zip` file.

## About

Inspired by https://github.com/cyxx/extract_android_ota_payload.
Forked from https://github.com/tobyxdd/android-ota-payload-extractor.
Further inspired by https://github.com/ssut/payload-dumper-go.

Just like the original project by [tobyxdd](https://github.com/tobyxdd/android-ota-payload-extractor) (inspired by [cyxx](https://github.com/cyxx/extract_android_ota_payload)), the goal of aota is to provide stable and high performance
OTA payload parsing so that you can utilize the data within. For example, most users will want to extract the boot image
from an OTA update so that they can install Magisk without a custom recovery. tobyxdd laid the groundwork so I could skip
the part where I reinvent a wheel again, but aota has more than just one goal!

The main driver for creating this fork was to multithread the OTA extraction process so that full OTA extractions would
take less time to finish on multi-core systems. Eventually I was told about [payload-dumper-go](https://github.com/ssut/payload-dumper-go) which already achieved this
goal, and I was already at the middle point between the slowness of the old project and the high speed of that project
just by spawning one goroutine per install operation and allowing any free CPU core to finish ops that aren't blocking.
So I took a hint on replacing the xz package and pre-extracting the payload to disk from ZIP files, replaced the bzip2
package, and did some logic magic to gain a whopping 3 seconds over payload-dumper-go. I was achieving 1min13s on a 16x
AMD EPYC with 32GB of RAM (which was particularly unbusy) versus 1min16s on the same system. However, before I was
inspired to make those fixes, I was reaching that same 1min16s on my Pixel 6 Pro after only moving to extracting the
payload to disk instead of using RAM to store it. Now it's taking longer (up to 1min50s) despite the consistent 3 second
gain on a VPS that isn't much faster, just slightly less busy.

I will likely update this about section as time goes on, but I can at least state the main goal of aota now that I've
reached my desired performance (with exception to the bug running on real Android with timings):

aota is taking all of this logic and converting it into a Go package so any program can make use of it. Import it into
your Go code, compile it as a shared library so it can be linked to other languages, or make use of the aota cmd wrapper
that implements every feature.

If you do something cool or interesting with aota, I may link it here! But for now, feel free to check out appOTA, my
only official project (as of writing) that implements aota as a Go package import! My future goal is to reach feature
parity with the Android Open Source Project's update_engine so that I can use aota inside of appOS Updater, which will
allow me to further optimize and customize the install process of OTAs.

[appOTA - the open source Android Plus Project OTA server](https://github.com/AndroidPlusProject/appOTA) (compatible with appOS/hentaiOS Updater, and also written in Go!)
