
build:
	go mod download
	go build -a -o cert-watcher

run: build
	./cert-watcher --secret-name=curl-test-tls --deployment-name=curl-test --namespace=flux-system --inside-cluster=false
