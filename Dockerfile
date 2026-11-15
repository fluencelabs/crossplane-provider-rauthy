# The controller image. The binary is cross-compiled on the build platform, so
# building for another architecture needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:5e8afd493a7124a8586e134d9b532731161d4b9becf9dee5a5956b107426f93d AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY apis/ apis/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/fluencelabs/crossplane-provider-rauthy/internal/version.Version=${VERSION}" \
    -o /out/provider ./cmd/provider

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

COPY --from=build /out/provider /usr/local/bin/crossplane-provider-rauthy

USER 65532:65532
ENTRYPOINT ["crossplane-provider-rauthy"]
