// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The UptimeCluster Authors
//
// Tiny HTTP target for the E2E monitors. Started as a child process by
// global-setup.ts. GET /__down makes it answer 503, GET /__up restores 200.

import http from 'node:http';

const port = Number(process.argv[2] ?? 28491);
let down = false;

const server = http.createServer((req, res) => {
  if (req.url === '/__down' || req.url === '/__up') {
    down = req.url === '/__down';
    res.writeHead(200, { 'content-type': 'text/plain' }).end(down ? 'down\n' : 'up\n');
    return;
  }
  res
    .writeHead(down ? 503 : 200, { 'content-type': 'text/plain' })
    .end(down ? 'unavailable\n' : 'ok\n');
});

server.listen(port, '127.0.0.1');

process.on('SIGTERM', () => {
  server.close(() => process.exit(0));
});
