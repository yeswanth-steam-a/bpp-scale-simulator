.PHONY: build test plan seed smoke
build:
	go build -o bin/simulator ./cmd/simulator
	go build -o bin/seedgen ./cmd/seedgen
	go build -o bin/mockcsms ./cmd/mockcsms
	go build -o bin/probe ./cmd/probe
test:
	go vet ./... && go test ./...
plan:
	go run ./cmd/simulator -plan
seed:
	go run ./cmd/seedgen -out seed
# 2000 chargers, 24h compressed to ~2 min, against the local mock CSMS
smoke: build
	./bin/mockcsms -addr :19898 & sleep 1; ./bin/simulator -target ws://127.0.0.1:19898/csms/ -chargers 2000 -time-scale 720; pkill mockcsms
