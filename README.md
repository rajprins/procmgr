# Procmgr

**Procmgr** is an interactive CLI for managing processes on MacOS.

<img width="927" height="574" alt="Image" src="https://github.com/user-attachments/assets/70db375b-e1d6-488e-8098-036052a4a420" />

## Features
procmgr allows you to:

- List processes and their memory usage
- Sort the process list by name (A-Z, Z-A) or memory usage (high-low, low-high)
- Search for processes
- Terminate one or more selected processes
- Navigate using keyboard


## Prequisites

If you wish to build `procmgr` from source, make sure you have:

- [Go](https://go.dev/dl/) version `1.25` or higher installed.
- An active internet connection (required for downloading dependencies)

## Build instructions

### Get the code

Create a clone of the repository:

```bash
git clone https://github.com/rajprins/procmgr
cd procmgr
```

### Get dependencies

This cleans and updates `go.mod` and `go.sum` by ensuring they reflect the actual dependencies required by the code.

```bash
go mod tidy
```

### Build the package

Build the `procmgr` package in the current directory:  

```bash
go build
```

Optionally, install the `procmgr` binary to ${GOPATH}/bin:  

```bash
go install
```

## Use of third-party packages

Procmgr depends on the following packages:  

- [go-ps](https://github.com/mitchellh/go-ps)
- [ts](github.com/olekukonko/ts)
