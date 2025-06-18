build-linux-amd64:
	mkdir -p dist
	cd ./src && ../sh/build -o linux -a amd64
