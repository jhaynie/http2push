# HTTP2 Push Proxy

> This is work in progress and should be ready for prime time in the next week if not earlier.  -2/6/2017

This server will acts as a [HTTP2 Push](https://en.wikipedia.org/wiki/HTTP/2_Server_Push) Proxy (and regular transparent proxy) and is meant to sit in from of an app server such as Node.JS.  This works great for web apps that use frameworks like [WebPack](http://webpack.github.io).

The goal is to accelerate static resource serving by:

- return in-memory cached static assets (the amount of in-memory RAM cache is configurable)
- perform HTTP2 Push for resources that are immediately necessarily in the served resource
- delegate any unhandled resources to the configured backend

## Caching

Any static resource found in the configured document root will be cached in memory and served to the client as requested. All cached resources will be gzipped (if the client supports) and will serve with ETag and Cache-Control HTTP headers.

## HTTP2 Push

This server will read HTML resources and parse them for `<link rel="push" href="...">` or `<link rel="prefetch" href="...">` elements.  Any element found in the HTML page will be pushed using HTTP2 Push to the client (if the client accepts HTTP2 Push).  The can dramatically speed up page loading and rendering in the browser.

## Backend

Any URL not found or not handled will automatically be reverse proxied to the configured backend for processing.

## Requirements

This code requires Go 1.8+ to take advantage of a couple of new features.  However, the Makefile runs against a 1.8+ version to compile the binary so you should be good.

## License

Copyright (c) 2017 by Jeff Haynie. Licensed under the [MIT](LICENSE) license.
