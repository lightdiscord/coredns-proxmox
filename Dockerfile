FROM golang:1.26-alpine AS build

WORKDIR /go/src/github.com/lightdiscord/coredns-proxmox

COPY go.mod go.sum ./
RUN go mod download

COPY --parents **/*.go ./

RUN CGO_ENABLED=0 go build -buildvcs=false -o /go/bin/coredns-proxmox ./cmd/coredns-proxmox

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /go/bin/coredns-proxmox /
ENTRYPOINT ["/coredns-proxmox"]