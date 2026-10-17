// Test-process guard. Fail closed before a Node child can reach installed services
// or write/read the prohibited production roots. No secrets are recorded.
const fs = require('node:fs');
const path = require('node:path');
const net = require('node:net');
const { fileURLToPath } = require('node:url');
const roots = JSON.parse(process.env.AGENTCTL_FORBIDDEN_ROOTS || '[]');
const append = fs.appendFileSync.bind(fs);
function fail(reason) {
  append(process.env.AGENTCTL_NETWORK_VIOLATIONS, reason + '\n');
  throw new Error('Isolated run blocked ' + reason);
}
function check(value) {
  if (value instanceof URL) value = fileURLToPath(value);
  if (Buffer.isBuffer(value)) value = value.toString();
  if (typeof value !== 'string') return;
  const resolved = path.resolve(value);
  if (roots.some(root => resolved === root || resolved.startsWith(root + path.sep))) {
    // Unit-suite fixture mode redirects unconfigured home defaults into a
    // process-private fake filesystem. Live acceptance never enables this.
    if (process.env.AGENTCTL_TEST_FS_ROOT) return path.join(process.env.AGENTCTL_TEST_FS_ROOT, String(process.pid), resolved);
    fail('forbidden filesystem path ' + resolved);
  }
  return value;
}
for (const name of ['access','appendFile','chmod','chown','copyFile','cp','createReadStream','createWriteStream','exists','lchmod','lchown','link','lstat','mkdir','mkdtemp','open','opendir','readFile','readdir','readlink','realpath','rename','rm','rmdir','stat','symlink','truncate','unlink','utimes','watch','watchFile','writeFile']) {
  for (const method of [name, name + 'Sync']) {
    const original = fs[method];
    if (typeof original !== 'function') continue;
    fs[method] = function(...args) {
      args[0] = check(args[0]) ?? args[0];
      if (['copyFile','cp','link','rename','symlink'].includes(name)) args[1] = check(args[1]) ?? args[1];
      return original.apply(this,args);
    };
    Object.assign(fs[method], original);
    if (name === 'realpath' && original.native) {
      fs[method].native = function(...args) { args[0] = check(args[0]) ?? args[0]; return original.native.apply(this,args); };
    }
  }
  const original = fs.promises[name];
  if (typeof original === 'function') fs.promises[name] = async function(...args) {
    args[0] = check(args[0]) ?? args[0];
    if (['copyFile','cp','link','rename','symlink'].includes(name)) args[1] = check(args[1]) ?? args[1];
    return original.apply(this,args);
  };
}
const connect = net.Socket.prototype.connect;
net.Socket.prototype.connect = function(...args) {
  let a = args[0]; if (Array.isArray(a)) a = a[0];
  const port = Number(typeof a === 'object' && a !== null ? a.port : a);
  if ([8803,38093].includes(port)) fail('forbidden port ' + port);
  return connect.apply(this,args);
};
require('node:module').syncBuiltinESMExports();
