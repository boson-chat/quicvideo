.PHONY: setup generate proto test vet run pub pub-h264 sub loadtest testdata docker netem

setup:
	go mod download
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12

generate: proto

proto:
	protoc -I proto --go_out=. --go_opt=module=github.com/boson-chat/quicvideo proto/control.proto

test:
	go test -race ./...

vet:
	go vet ./...

# Local server with a self-signed cert and no auth.
run:
	AUTH_INSECURE=true go run ./server/cmd serve

name ?= alice
room ?= call
addr ?= localhost:7710

# Synthetic 3-layer camera: make pub name=alice
pub:
	go run ./examples/pub -insecure -addr $(addr) -token $(name) -room $(room)

# Real H.264 looped from testdata (built by `make testdata`).
pub-h264: testdata
	go run ./examples/pub -insecure -addr $(addr) -token $(name) -room $(room) \
		-layers 320x180@15:200k,640x360@30:600k,1280x720@30:1500k \
		-h264 testdata/l0.h264,testdata/l1.h264,testdata/l2.h264

# Viewer: make sub name=bob layer=2 dump=out.h264, then ffplay -f h264 out.h264
layer ?= 0
dump ?=
sub:
	go run ./examples/sub -insecure -addr $(addr) -token $(name) -room $(room) -layer $(layer) $(if $(dump),-dump $(dump))

rooms ?= 4
n ?= 6
loadtest:
	go run ./examples/loadtest -insecure -addr $(addr) -rooms $(rooms) -n $(n)

# Three simulcast layers of ffmpeg's test pattern with a 2 s GOP and access
# unit delimiters (examples/pub splits frames on them).
X264 = -c:v libx264 -preset veryfast -tune zerolatency -profile:v baseline -pix_fmt yuv420p -t 10
AUD = -bsf:v h264_metadata=aud=insert -f h264

testdata: testdata/l0.h264 testdata/l1.h264 testdata/l2.h264

testdata/l0.h264:
	@mkdir -p testdata
	ffmpeg -loglevel error -y -f lavfi -i testsrc2=size=320x180:rate=15 $(X264) \
		-b:v 200k -maxrate 200k -bufsize 200k -x264-params keyint=30:min-keyint=30:scenecut=0:repeat-headers=1 $(AUD) $@

testdata/l1.h264:
	@mkdir -p testdata
	ffmpeg -loglevel error -y -f lavfi -i testsrc2=size=640x360:rate=30 $(X264) \
		-b:v 600k -maxrate 600k -bufsize 600k -x264-params keyint=60:min-keyint=60:scenecut=0:repeat-headers=1 $(AUD) $@

testdata/l2.h264:
	@mkdir -p testdata
	ffmpeg -loglevel error -y -f lavfi -i testsrc2=size=1280x720:rate=30 $(X264) \
		-b:v 1500k -maxrate 1500k -bufsize 1500k -x264-params keyint=60:min-keyint=60:scenecut=0:repeat-headers=1 $(AUD) $@

docker:
	docker build -t quicvideod:dev .

# Bad-network scenarios in Docker (tc netem/tbf); results in netem/results/results.md
netem:
	netem/run.sh
