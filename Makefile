.PHONY: test lint clean fmt tidy ci coverage

test:
	go test -v ./...

lint:
	golangci-lint run ./...

clean:
	go clean
	rm -f coverage.out coverage.html

fmt:
	go fmt ./... && git diff --exit-code

tidy:
	go mod tidy && git diff --exit-code

ci: tidy fmt lint test
	@echo
	@echo "\033[32mEVERYTHING PASSED!\033[0m"

coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"
