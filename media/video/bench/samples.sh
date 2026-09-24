#!/bin/sh
# Writes the encode benchmark's synthetic sources into $1 (default .):
# testsrc2 with temporal noise and a hue sweep, x264 CRF 18 + stereo AAC.
set -eu
dir=${1:-.}
mkdir -p "$dir"
gen() { # name w h seconds ext
	[ -f "$dir/$1.$5" ] && return
	nice -n 19 ffmpeg -v error -nostdin -f lavfi -i "testsrc2=s=$2x$3:r=30:d=$4,noise=alls=7:allf=t+u,hue=H=0.2*t" \
		-f lavfi -i "sine=f=330:d=$4:sample_rate=48000" -ac 2 -c:v libx264 -preset veryfast -crf 18 -pix_fmt yuv420p \
		-threads 8 -c:a aac -b:a 160k -y "$dir/$1.$5"
}
gen portrait31 1080 1920 31 mov
gen hd300 1920 1080 300 mp4
gen uhd120 3840 2160 120 mkv
