import http from 'node:http';

/**
 * Local mock provider server simulating an OpenAI-compatible upstream API.
 * Listens on an ephemeral port (127.0.0.1:0) to prevent port collisions.
 */
export class MockProvider {
  constructor() {
    this.server = null;
    this.port = 0;
    this.url = '';
    this.requests = [];
  }

  async start() {
    return new Promise((resolve, reject) => {
      this.server = http.createServer((req, res) => {
        let body = '';
        req.on('data', (chunk) => {
          body += chunk;
        });

        req.on('end', () => {
          let parsed = {};
          try {
            parsed = JSON.parse(body);
          } catch {
            // non-json
          }

          this.requests.push({
            method: req.method,
            url: req.url,
            headers: req.headers,
            body: parsed
          });

          // Handle OpenAI chat completions
          if (req.url && req.url.includes('/chat/completions')) {
            const isStreaming = Boolean(parsed && parsed.stream);

            if (isStreaming) {
              res.writeHead(200, {
                'Content-Type': 'text/event-stream; charset=utf-8',
                'Cache-Control': 'no-cache',
                Connection: 'keep-alive'
              });

              const chunks = [
                { id: 'chatcmpl-mock-1', choices: [{ delta: { role: 'assistant', content: 'Still' } }] },
                { id: 'chatcmpl-mock-2', choices: [{ delta: { content: 'Mark' } }] },
                { id: 'chatcmpl-mock-3', choices: [{ delta: { content: ' verified' } }] },
                {
                  id: 'chatcmpl-mock-4',
                  choices: [{ delta: {} }],
                  usage: {
                    prompt_tokens: 100,
                    completion_tokens: 3,
                    total_tokens: 103,
                    prompt_tokens_details: {
                      cached_read_tokens: 80
                    }
                  }
                }
              ];

              for (const c of chunks) {
                res.write(`data: ${JSON.stringify(c)}\n\n`);
              }
              res.write('data: [DONE]\n\n');
              res.end();
              return;
            }

            // Unary JSON completion
            const responseData = {
              id: 'chatcmpl-mock-unary',
              object: 'chat.completion',
              created: Math.floor(Date.now() / 1000),
              model: parsed.model || 'mock-model',
              choices: [
                {
                  index: 0,
                  message: {
                    role: 'assistant',
                    content: 'StillMark mock response'
                  },
                  finish_reason: 'stop'
                }
              ],
              usage: {
                prompt_tokens: 120,
                completion_tokens: 10,
                total_tokens: 130,
                prompt_tokens_details: {
                  cached_read_tokens: 95
                }
              }
            };

            const respBytes = Buffer.from(JSON.stringify(responseData));
            res.writeHead(200, {
              'Content-Type': 'application/json',
              'Content-Length': respBytes.length
            });
            res.end(respBytes);
            return;
          }

          // Fallback 404
          res.writeHead(404, { 'Content-Type': 'text/plain' });
          res.end('Not Found');
        });
      });

      this.server.on('error', reject);

      this.server.listen(0, '127.0.0.1', () => {
        const addr = this.server.address();
        this.port = addr.port;
        this.url = `http://127.0.0.1:${this.port}`;
        resolve(this);
      });
    });
  }

  async stop() {
    if (!this.server) return;
    return new Promise((resolve) => {
      this.server.close(() => {
        this.server = null;
        resolve();
      });
    });
  }

  clearRequests() {
    this.requests = [];
  }
}

