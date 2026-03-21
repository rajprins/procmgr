# Procmgr

**Procmgr** is an interactive CLI for managing processes on MacOS.
<img width="1060" height="798" alt="Image" src="https://github.com/user-attachments/assets/3750ecec-9d17-4ba3-b150-3b876a0c31c7" />

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

## Screenshots  

Searching using `/`  
<img width="1060" height="798" alt="Image" src="https://github.com/user-attachments/assets/64fa049d-f6e6-4645-9a65-7f0af1abc0e5" />

Selecting multiple processes  
<img width="1060" height="798" alt="Image" src="https://github.com/user-attachments/assets/07484a12-a782-4c8e-af51-fa98eec65205" />

Terminating multiple processes  
<img width="1060" height="798" alt="Image" src="https://github.com/user-attachments/assets/771c9272-6052-40b1-9bcd-9881364655a6" />

