const fs = require('node:fs');
const vm = require('node:vm');
const util = require('node:util');
const { Writable } = require('node:stream');
const { spawn } = require('node:child_process');
const pipeRetryMS = 1;
const retrySignal = new Int32Array(new SharedArrayBuffer(4));
const killGroup = process.kill.bind(process, -process.pid, 'SIGKILL');

// A separate process can observe harness death without blocking runtime exit.
// It stays in our group, so the group cannot be reused before lifetime EOF.
const watchdog = spawn(process.execPath, ['-e', `
  require('node:fs').readSync(3, Buffer.alloc(1), 0, 1, null);
  process.kill(-${process.pid}, 'SIGKILL');
`], {stdio: ['ignore', 'ignore', 'ignore', 5]});
watchdog.on('error', killGroup);
watchdog.on('exit', killGroup);
watchdog.unref();
fs.closeSync(5);
process.on('uncaughtException', killGroup);
process.on('unhandledRejection', killGroup);

function writeAll(fd, bytes) {
  let offset = 0;
  while (offset < bytes.length) {
    try {
      const count = fs.writeSync(fd, bytes, offset, bytes.length - offset);
      if (count === 0) throw new Error('eval output pipe stopped accepting bytes');
      offset += count;
    } catch (error) {
      if (error.code !== 'EAGAIN' && error.code !== 'EINTR') throw error;
      // A native module may set O_NONBLOCK on the shared pipe description.
      Atomics.wait(retrySignal, 0, 0, pipeRetryMS);
    }
  }
}
function outputStream(fd) {
  const output = new Writable({write(chunk, encoding, callback) {
    try { writeAll(fd, chunk); callback(); }
    catch (error) { callback(error); }
  }});
  output.on('error', killGroup);
  return output;
}
// Never read Node's lazy stdout/stderr getters: they make the pipes nonblocking.
const stdout = outputStream(1);
const stderr = outputStream(2);
Object.defineProperty(process, 'stdout', {value: stdout});
Object.defineProperty(process, 'stderr', {value: stderr});
const { Console } = require('node:console');
const cellConsole = new Console({stdout, stderr, ignoreErrors: false});

async function readExact(size) {
  const bytes = Buffer.alloc(size);
  let offset = 0;
  while (offset < size) {
    const count = await new Promise((resolve, reject) => {
      fs.read(3, bytes, offset, size - offset, null, (error, count) => {
        if (error) reject(error);
        else resolve(count);
      });
    });
    if (count === 0) return null;
    offset += count;
  }
  return bytes;
}
function respond(result) {
  const payload = Buffer.from(JSON.stringify(result));
  const header = Buffer.alloc(4);
  header.writeUInt32BE(payload.length);
  writeAll(4, header);
  writeAll(4, payload);
}
function describeError(error) {
  let code = 'Error';
  let text = '<unprintable thrown value>';
  try {
    if (error !== null && error !== undefined) {
      if (typeof error.code === 'string' && error.code.length > 0) code = error.code;
      else if (typeof error.name === 'string' && error.name.length > 0) code = error.name;
    }
    text = error && typeof error.stack === 'string' ? error.stack : util.inspect(error, {customInspect: false, getters: false});
  } catch (formatError) {
    // Even hostile getters/proxies must leave a failed, settled cell.
    text = '<unprintable thrown value; error formatting failed>';
  }
  return {code, text};
}
const namespace = vm.createContext({console: cellConsole, require, process, Buffer,
  setTimeout, clearTimeout, setInterval, clearInterval, URL, TextEncoder, TextDecoder});
process.on('SIGINT', () => { /* Idle interrupts must not terminate the kernel. */ });
async function main() {
  respond({event: 'ready'});
  while (true) {
    const header = await readExact(4);
    if (header === null) return;
    const payload = await readExact(header.readUInt32BE());
    if (payload === null) throw new Error('eval request ended within a frame');
    const request = JSON.parse(payload.toString('utf8'));
    let cellError = '';
    respond({event: 'started'});
    try {
      const script = new vm.Script(request.code, {filename: '<eval>'});
      const options = {breakOnSigint: true};
      if (request.timeout_ms > 0) options.timeout = request.timeout_ms;
      const value = script.runInContext(namespace, options);
      if (value !== undefined) writeAll(1, Buffer.from(util.inspect(value) + '\n'));
    } catch (error) {
      const description = describeError(error);
      cellError = description.code;
      writeAll(2, Buffer.from(description.text + '\n'));
    }
    writeAll(1, Buffer.from(request.boundary));
    respond({event: 'done', error: cellError});
  }
}
// Normal EOF must also stop the watchdog and user-created descendants.
main().then(killGroup, killGroup);
