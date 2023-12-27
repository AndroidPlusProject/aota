# aota

A multithreaded Android OTA installer, written in Go.

[Download self-hosted releases from the latest commit](https://files.joshuadoes.com/aota) (stripped using `go build -ldflags="-s -w"`)

## Usage

On Linux, Mac, and pretty much everything else:

`./aota -a value` or `./aota --arg value`

On Windows:

`.\aota.exe -a value` or `.\aota.exe --arg value`

```
--input <required: path to OTA.zip or payload.bin>
--out <optional: path to output folder>
--extract <optional: comma,separated,partition,list>
--cap <optional: memory limit in bytes for buffering install operations, anything above 64MiB is pointless>
--jobs <optional: limit or increase the number of Go processes that can spawn>
--debug <optional: no value, just shows debug logs when used>
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
AMD EPYC with 32GB of RAM (which was particularly unbusy) versus 1min16s on the same system.

Due to the nature of app environments within Android, it's been rather difficult to test aota in an accurate manner on
my Pixel 6 Pro. But after some further bugfixes, if I use a payload.bin directly and already have the space allocated
for the partition images (simulating a streamed install but without the networking), aota is now reaching 1min19s on
the Pixel 6 Pro within Termux! There's no certainty in which CPU core will be executing a given install operation, so
it can vary mildly, but it hasn't gone above 1min33s yet during my own testing. I miss the 1min16s mark that I reached
before on this same phone, but it's hard to argue with the improvements across the board.

I will likely update this about section as time goes on, but I can at least state the main goal of aota now that I've
reached my desired performance (with exception to the bug running on real Android with timings):

aota is taking all of this logic and converting it into a Go package so any program can make use of it. Import it into
your Go code, compile it as a shared library so it can be linked to other languages, or make use of the aota cmd wrapper
that implements every feature.

---

If you do something cool or interesting with aota, I may link it here!

---

### appOTA

[appOTA - the open source Android Plus Project OTA server](https://github.com/AndroidPlusProject/appOTA)

appOTA was created as a reverse-engineering effort of [hentaiOS Updater](https://github.com/hentaiOS/platform_packages_apps_Updater) to allow it to be used in appOS and other distributions of Android.

appOTA uses aota to parse and manipulate OTA payloads before serving them to the Updater app. It is intended to serve OTAs to AOSP's update_engine service.

appOS and appOTA are not yet available to consumers, check back shortly!

---
