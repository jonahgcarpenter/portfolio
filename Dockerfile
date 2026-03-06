# We use the official Golang Alpine image just for compiling.
# Alpine is lightweight and fast for CI/CD pipelines.
FROM golang:1.25-alpine AS builder

# Install SSL CA certificates. 
# (Crucial if your Go app ever needs to make secure HTTPS requests)
RUN apk update && apk add --no-cache ca-certificates

# Create a non-root user and group.
# We will copy these into the final scratch container for security.
RUN adduser -D -g '' appuser

# Set the working directory inside the container
WORKDIR /app

# Copy the Go module files first to leverage Docker layer caching.
# This makes subsequent builds much faster if your dependencies haven't changed.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY ./main.go .
COPY ./index.html .

# Compile the Go application.
# CGO_ENABLED=0 is the magic flag: it tells Go to build a statically linked binary 
# that does NOT require any external C libraries (like glibc) to run.
# -ldflags="-w -s" strips debugging information, shrinking the file size even further.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o portfolio .


# 'scratch' is a special Docker keyword for a completely empty, 0-byte image.
# It has no OS, no shell, no package manager—nothing. Zero CVEs.
FROM scratch

# Add the OCI Label here
LABEL org.opencontainers.image.source="https://github.com/jonahgcarpenter/portfolio"

# Import the CA certificates from the builder stage
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Import the non-root user and group files from the builder stage
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /etc/group /etc/group

# Copy the statically linked compiled Go binary from the builder stage
COPY --from=builder /app/portfolio /portfolio

# Switch to the non-root user for extreme security (Principle of Least Privilege)
USER appuser:appuser

# Tell Docker that the container listens on port 8080
EXPOSE 8080

# Execute the binary
ENTRYPOINT ["/portfolio"]
