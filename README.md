# SimpleAmazon

A frontend for Amazon that allows users to browse and search amazon on any TLD of choice (like fr, de, com, co.uk, etc)

**NOTE**: It needs golang version 1.16 and above. so if you use debian 11, you need install golang using `wget` not debian's package manager.
## How to install/run

```bash
go install codeberg.org/SimpleWeb/SimpleAmazon@latest
SimpleAmazon
```

**OR**

```bash
git clone https://codeberg.org/SimpleWeb/SimpleAmazon.git
cd SimpleAmazon
go run main.go
```
then you can use flags like `-h` or `-p` as needed.

## Usage

You can only specify the `host` and the `port` the program should run on using the `-h` and the `-p` flag respectively. Default values are `host:localhost` and `port:8080`


## Update

In case of you used second method, you can `git pull` to fetch new updates and re-run `go run main.go` with updated codes

