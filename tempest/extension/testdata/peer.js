const readline = require('node:readline');

class Peer {
  constructor() {
    this.sequence = 0;
    this.pending = new Map();
  }

  send(frame) {
    process.stdout.write(JSON.stringify(frame) + '\n');
  }

  callback(method, params) {
    const id = `callback-${++this.sequence}`;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.send({ jsonrpc: '2.0', id, method, params });
    });
  }

  async dispatch(frame) {
    if (!frame.method) {
      const pending = this.pending.get(frame.id);
      if (!pending) throw new Error('Unknown callback response');
      this.pending.delete(frame.id);
      if (frame.error) pending.reject(new Error(frame.error.message));
      else pending.resolve(frame.result);
      return;
    }
    if (frame.id === undefined) return;
    const params = frame.params || {};
    let result;
    if (frame.method === 'initialize') {
      result = {
        protocolVersion: 'tempest-extension/1', id: 'sample', revision: 'v1',
        capabilities: ['read', 'commit', 'memo', 'next'],
        definition: {
          sections: [{ name: 'instructions' }],
          tools: [{ name: 'echo', description: 'Echo a message', inputSchema: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'], additionalProperties: false }, outputSchema: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] }, effect: 'write', replay: 'safe' }],
          hooks: [{ name: 'approval', event: 'before_tool', replay: 'safe' }],
          wraps: [{ name: 'brackets', target: 'sample/echo', replay: 'safe' }],
          documents: [{ namespace: 'sample', version: 1, schema: { type: 'object', properties: { count: { type: 'integer' } }, required: ['count'] }, initial: { count: 0 }, history: 'rewindable', fork: 'asOf', reset: 'preserve' }],
        },
      };
    } else if (frame.method.endsWith('sections/render')) {
      result = { text: 'Extension section' };
    } else if (frame.method.endsWith('hooks/call')) {
      await this.callback('_tempest/extension/context/memo', { token: params.context.token, key: 'approval', candidate: true });
      result = { kind: 'continue' };
    } else if (frame.method.endsWith('tools/call')) {
      const identity = params.context;
      await this.callback('_tempest/extension/context/memo', { token: identity.token, key: 'false', candidate: false });
      await this.callback('_tempest/extension/context/memo', { token: identity.token, key: 'null', candidate: null });
      await this.callback('_tempest/extension/context/commit', { token: identity.token, commit: { id: `${identity.invocationId}:document`, changes: [{ sessionId: identity.sessionId, kind: 'document', namespace: 'sample', id: 'state', version: 1, value: { count: 1 } }] } });
      result = { content: [{ type: 'text', text: params.arguments.text }], structuredContent: { text: params.arguments.text } };
    } else if (frame.method.endsWith('wraps/call')) {
      result = await this.callback('_tempest/extension/context/next', { token: params.context.token });
      result.content[0].text = `[${result.content[0].text}]`;
      result.structuredContent.text = `[${result.structuredContent.text}]`;
    } else if (frame.method.endsWith('shutdown')) {
      result = {};
    } else {
      throw new Error('Unknown method');
    }
    this.send({ jsonrpc: '2.0', id: frame.id, result });
  }
}

const peer = new Peer();
const input = readline.createInterface({ input: process.stdin });
input.on('line', line => {
  peer.dispatch(JSON.parse(line)).catch(error => {
    process.stderr.write(error.message + '\n');
    process.exitCode = 1;
    input.close();
    process.stdin.destroy();
  });
});
