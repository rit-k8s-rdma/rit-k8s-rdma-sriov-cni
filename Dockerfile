FROM golang:1.10.1 as build

WORKDIR /go/workspace
COPY . .

ENV GOPATH=/go/workspace
ENV CGO_ENABLED=0
ENV GOOS=linux
RUN go install -ldflags="-s -w -X main.GitCommitId=$GIT_COMMIT -extldflags "-static"" -v sriov
RUN go install -ldflags="-s -w -X main.GitCommitId=$GIT_COMMIT -extldflags "-static"" -v fixipam

FROM debian:stretch-slim
COPY --from=build /go/workspace/bin/sriov /bin/sriov
COPY --from=build /go/workspace/bin/fixipam /bin/fixipam
