# Grafana Loki Client for Go

## Table of Contents

1. [Introduction](#introduction)
2. [Installation](#installation)
3. [Quick Start](#quick-start)
4. [Core Concepts](#core-concepts)
   - [Stream](#stream)
   - [Value](#value)
   - [Entry](#entry)
5. [Client Configuration](#client-configuration)
   - [Creating a New Client](#creating-a-new-client)
   - [Client Options](#client-options)
6. [Logging Methods](#logging-methods)
7. [Synchronization](#synchronization)
8. [Batching and Performance](#batching-and-performance)
9. [Error Handling and Retries](#error-handling-and-retries)
10. [Fallback Loggers](#fallback-loggers)
11. [HTTP Client Customization](#http-client-customization)
12. [Advanced Usage](#advanced-usage)

## Introduction

This Go package provides a client for Grafana Loki, a horizontally-scalable, highly-available, multi-tenant log aggregation system. The client allows you to send log entries to a Loki server with various configuration options for performance and reliability.

## Installation

To install the Loki client, use the following go get command:

```
go get github.com/yourusername/lokiclient
```

Replace `yourusername` with the actual path to the package.

## Quick Start

Here's a simple example to get you started:

```go
package main

import (
    "context"
    "github.com/yourusername/lokiclient"
    "time"
)

func main() {
    client := lokiclient.NewClient("http://loki:3100/loki/api/v1/push")
    
    stream := lokiclient.NewStream("myapp", "module1", "function1", "trace123")
    value := lokiclient.NewValue("This is a log message", "key1", "value1")
    
    client.Info(stream, value)
    
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    
    client.Sync(ctx)
    
    // Your application code here
    time.Sleep(5 * time.Second)
}
```

## Core Concepts

### Stream

A `Stream` represents the metadata associated with a log entry. It's implemented as a `map[string]string`.

```go
stream := lokiclient.NewStream("myapp", "module1", "function1", "trace123")
```

You can also create a custom stream:

```go
customStream := lokiclient.NewStreamCustom(map[string]string{
    "app": "myapp",
    "environment": "production",
    "server": "server01",
})
```

### Value

A `Value` represents the actual log data. It's implemented as a slice of `interface{}`.

```go
value := lokiclient.NewValue("Log message", "item1", "item2")
```

### Entry

An `Entry` combines a `Stream` and a `Value`. It's the basic unit of data sent to Loki.

## Client Configuration

### Creating a New Client

Create a new Loki client by specifying the Loki server address:

```go
client := lokiclient.NewClient("http://loki:3100/loki/api/v1/push")
```

### Client Options

You can customize the client behavior using the following options:

#### WithRetry

Configure retry behavior:

```go
lokiclient.WithRetry(3, time.Second)
```

This sets a maximum of 3 retries with a 1-second pause between retries.

#### WithBatchSync

Enable batched synchronization:

```go
lokiclient.WithBatchSync(100, 5*time.Second)
```

This batches log entries, sending them when either 100 entries are accumulated or 5 seconds have passed.

#### WithFallback

Add a fallback logger:

```go
lokiclient.WithFallback(myFallbackLogger)
```

This adds a fallback logger to use if sending to Loki fails.

## Logging Methods

The client provides methods for different log levels:

- `Trace(stream Stream, value Value)`
- `Debug(stream Stream, value Value)`
- `Info(stream Stream, value Value)`
- `Warn(stream Stream, value Value)`
- `Error(stream Stream, value Value)`

Example:

```go
client.Info(stream, lokiclient.NewValue("User logged in", "userId", 12345))
```

## Synchronization

Start the synchronization process:

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()
client.Sync(ctx)
```

This starts a goroutine that handles sending log entries to Loki.

## Batching and Performance

The client supports batching of log entries for improved performance. When batching is enabled, entries are grouped by their stream before being sent to Loki.

To enable batching:

```go
client := lokiclient.NewClient("http://loki:3100/loki/api/v1/push")
client.WithBatchSync(100, 5*time.Second)
```

## Error Handling and Retries

The client automatically retries failed requests based on the configured retry settings. If all retries fail, it will attempt to use any configured fallback loggers.

## Fallback Loggers

Fallback loggers are used when the client fails to send logs to Loki after all retries. You can add multiple fallback loggers:

```go
client.WithFallback(consoleLogger)
client.WithFallback(fileLogger)
```

## HTTP Client Customization

You can customize the HTTP client used by the Loki client:

```go
customHttpClient := &http.Client{
    Timeout: 10 * time.Second,
}
lokiclient.SetHttpClient(customHttpClient)
```

## Advanced Usage

### Custom Stream Creation

Create streams with custom labels:

```go
stream := lokiclient.NewStreamCustom(map[string]string{
    "app": "myapp",
    "environment": "production",
    "server": "server01",
})
```

### Manual Writing of Entries

You can manually write entries using the `Write` method:

```go
entries := []*lokiclient.Entry{
    {
        Stream: stream,
        Values: []lokiclient.Value{
            lokiclient.NewValue(time.Now().UnixNano(), "INFO", "Message 1"),
            lokiclient.NewValue(time.Now().UnixNano(), "ERROR", "Message 2"),
        },
    },
}
err := client.Write(entries)
if err != nil {
    // Handle error
}
```

This documentation covers the main features and usage of your Grafana Loki client. Let me know if you need any clarification or additional information on any part of the documentation.