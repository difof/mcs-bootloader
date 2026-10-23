#!/usr/bin/env python3
"""One-time upgrade from split configuration to .env and socket reload.

Preserves the existing credential and Java process. Only the gateway restarts.
A private historical archive is made before replacing any installation files.
"""

import argparse
import datetime
import grp
import hashlib
import io
import json
import os
from pathlib import Path
import pwd
import re
import shlex
import stat
import subprocess
import sys
import tarfile
import tempfile
import time

sys.dont_write_bytecode = True

from env_config import CONFIG_KEYS, MANAGE_DEFAULTS, RUNTIME_DEFAULTS, read_env, serialize_env, values_from_config
from install import write_atomic

ROOT = Path(__file__).resolve().parent.parent
CONFIG_DIR = Path('/etc/mc-gateway')
CANONICAL = CONFIG_DIR / '.env'
BINARY = Path('/opt/mc-gateway/bin/mc-gateway')
GATEWAY = 'mc-gateway.service'
BACKEND = 'mc-gateway-backend.service'


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def run(argv, timeout=30):
    result = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
    # Subprocess diagnostics are deliberately not echoed: a failing parser or
    # third-party tool must not copy configuration values into logs.
    require(result.returncode == 0, f'{Path(argv[0]).name} failed; migration stopped safely')
    return result.stdout.strip()


def service_property(service, name):
    return run(['/usr/bin/systemctl', 'show', service, '--property='+name, '--value'])


def process_identity(pid):
    """Kernel start time distinguishes an original process from PID reuse."""
    require(pid.isdecimal(), 'service returned an invalid process identifier')
    if int(pid) == 0:
        return None
    require(int(pid) > 1, 'service returned an unsafe process identifier')
    try:
        raw = (Path('/proc') / pid / 'stat').read_text()
    except OSError:
        raise RuntimeError('original service process exited during migration inspection') from None
    end = raw.rfind(')')
    fields = raw[end+1:].split() if end >= 0 else []
    require(len(fields) >= 20 and fields[0] != 'Z' and fields[19].isdecimal(),
            'original service process is not running or has invalid metadata')
    return fields[19]


def verify_process(service, pid, identity):
    require(service_property(service, 'MainPID') == pid and process_identity(pid) == identity,
            'original service process changed during migration; inspect before continuing')


def gateway_status(user, socket, timeout=5):
    try:
        result = subprocess.run(['/usr/sbin/runuser', '-u', user, '--', str(BINARY),
                                 'status', '--socket', str(socket)],
                                capture_output=True, text=True, timeout=timeout)
        if result.returncode != 0:
            return None
        result = json.loads(result.stdout)
        return result if isinstance(result, dict) else None
    except (OSError, subprocess.TimeoutExpired, json.JSONDecodeError):
        return None


def lifecycle_healthy(status, backend_was_running):
    lifecycle = status.get('lifecycle', {})
    if not isinstance(lifecycle, dict) or lifecycle.get('backend_known') is not True:
        return False
    if backend_was_running:
        players = lifecycle.get('players')
        return (lifecycle.get('state') == 'ready'
                and lifecycle.get('backend_running') is True
                and type(players) is int and players >= 0)
    return (lifecycle.get('state') == 'asleep'
            and lifecycle.get('backend_running') is False)


def verify_completed_installation(project_env):
    require(project_env.is_symlink() and project_env.resolve() == CANONICAL
            and CANONICAL.is_file() and not CANONICAL.is_symlink(),
            'canonical .env already exists but project link does not match; inspect before migrating')
    report_path = ROOT / '.local/env-migration.json'
    try:
        report = json.loads(report_path.read_text())
    except (OSError, json.JSONDecodeError):
        raise RuntimeError('canonical .env exists without a completed migration report; inspect the private archive and recover the partial migration') from None
    require(isinstance(report, dict) and report.get('status') == 'complete',
            'an earlier migration is incomplete; inspect its private report and archive before recovery')
    require(service_property(GATEWAY, 'ActiveState') == 'active',
            'migration report exists but the gateway is not active; inspect before declaring migration complete')
    values = read_env(CANONICAL)
    user = service_property(GATEWAY, 'User')
    status = gateway_status(user, values.get('MC_ADMIN_SOCKET', RUNTIME_DEFAULTS['MC_ADMIN_SOCKET']))
    require(status is not None and status.get('version') == report.get('version')
            and status.get('version') == run([str(BINARY), 'version']),
            'migration report and active/installed gateway versions do not match; inspect before continuing')
    require(status.get('configuration', {}).get('sha256') == hashlib.sha256(CANONICAL.read_bytes()).hexdigest(),
            'canonical .env differs from the active gateway; inspect pending edits or incomplete migration')


def read_legacy_env(path):
    data=path.read_text()
    require(len(data.encode()) <= 65536, 'legacy .env exceeds size limit')
    # The old password-file key is the sole retired dotenv key. Read all other
    # assignments with the exact same strict data parser used by new tasks.
    filtered='\n'.join(line for line in data.splitlines()
                       if not re.match(r'^\s*(?:export\s+)?MC_RCON_PASSWORD_FILE\s*=', line))+'\n'
    # This parser snapshot is private and temporary, including during --check.
    # Preflight never changes the installation directory.
    fd,name=tempfile.mkstemp(prefix='.mc-env-migrate-')
    try:
        with os.fdopen(fd,'w') as stream:
            stream.write(filtered)
        return read_env(Path(name))
    finally:
        Path(name).unlink(missing_ok=True)


def render_unit(name,user,group):
    text=(ROOT/'deploy'/name).read_text()
    require(text.count('User=minecraft\n') == 1 and text.count('Group=minecraft\n') == 1,
            'service template account markers changed')
    text=text.replace('User=minecraft\n',f'User={user}\n').replace('Group=minecraft\n',f'Group={group}\n')
    require('/etc/mc-gateway/.env' in text and 'config.json' not in text and 'backend.env' not in text,
            'service templates do not use the canonical .env')
    return text.encode()


def validate_candidate(source_bytes, env_data, unit_names, units, version):
    """Exercise exact candidate bytes using disposable, private files only."""
    with tempfile.TemporaryDirectory(prefix='mc-env-preflight-') as temporary:
        directory = Path(temporary)
        binary, env = directory / 'mc-gateway', directory / '.env'
        binary.write_bytes(source_bytes);binary.chmod(0o700)
        env.write_bytes(env_data);env.chmod(0o600)
        require(run([str(binary), 'version']) == version,
                'candidate executable changed during preflight; rebuild and retry')
        run([str(binary), 'check', '--config', str(env)])
        paths = []
        for name, data in zip(unit_names, units):
            path = directory / name
            path.write_bytes(data)
            paths.append(str(path))
        run(['/usr/bin/systemd-analyze', 'verify', *paths])


def snapshot_archive(path, files, running_executable, migration_metadata):
    fd=os.open(path,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
    with os.fdopen(fd,'wb') as stream:
        with tarfile.open(fileobj=stream,mode='w:gz') as archive:
            for source in files:
                if source.exists() or source.is_symlink():
                    archive.add(source,arcname=str(source).lstrip('/'),recursive=False)
            # /proc/PID/exe is a symlink, often to a deleted old executable.
            # Store its captured bytes as a regular archive member, not a link.
            for name, data, mode in (
                    ('migration/running-gateway', running_executable, 0o755),
                    ('migration/metadata.json', (json.dumps(migration_metadata, indent=2)+'\n').encode(), 0o600)):
                entry = tarfile.TarInfo(name)
                entry.size, entry.mode, entry.mtime = len(data), mode, int(time.time())
                archive.addfile(entry, io.BytesIO(data))
        stream.flush();os.fsync(stream.fileno())
    directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def migrate(check_only=False):
    require(os.geteuid()==0,'migration must run as root')
    project_env=ROOT/'.env'
    if CANONICAL.exists() or CANONICAL.is_symlink():
        verify_completed_installation(project_env)
        print('Single .env configuration already installed; use task config:apply for settings or task deploy for code.')
        return
    require(project_env.is_file() and not project_env.is_symlink(),'legacy project .env must be a regular file')
    metadata=project_env.stat()
    require(stat.S_IMODE(metadata.st_mode) in (0o600,0o640),'legacy project .env must be private')
    owner=pwd.getpwuid(metadata.st_uid)
    require(owner.pw_uid!=0,'project .env must belong to the operator account')
    user=service_property(GATEWAY,'User')
    require(user==service_property(BACKEND,'User'),'gateway and backend service accounts differ')
    require(re.fullmatch(r'[a-z_][a-z0-9_-]*\$?',user) is not None,'unsupported service account')
    account=pwd.getpwnam(user);group=grp.getgrgid(account.pw_gid).gr_name
    require(account.pw_uid!=0,'gateway must run as a non-root service account')
    require(service_property(GATEWAY,'ActiveState')=='active','start the existing gateway before migration')
    before_pid=service_property(BACKEND,'MainPID')
    before_identity=process_identity(before_pid)
    backend_state=service_property(BACKEND,'ActiveState')
    require((before_identity is not None and backend_state=='active')
            or (before_identity is None and backend_state in ('inactive','failed')),
            'Minecraft is changing service state; wait for a stable active or stopped state before migration')
    gateway_pid=service_property(GATEWAY,'MainPID')
    gateway_identity=process_identity(gateway_pid)
    require(gateway_identity is not None,'active gateway has no original process')
    running_executable_path=Path('/proc')/gateway_pid/'exe'
    running_executable=running_executable_path.read_bytes()
    require(running_executable.startswith(b'\x7fELF'),'running gateway executable could not be preserved')
    running_version=run([str(running_executable_path),'version'])
    old_config_path=CONFIG_DIR/'config.json'
    old_config=json.loads(old_config_path.read_text())
    password_path=Path(old_config.pop('rcon_password_file'))
    require(password_path==CONFIG_DIR/'rcon.password','unexpected legacy password location; inspect installation')
    # Match the old Go ReadPassword normalization. The stored effective secret,
    # rather than incidental password-file whitespace, must survive migration.
    password=password_path.read_text().strip()
    require(len(password)>=16 and not any(c in password for c in '\r\n\0'),'legacy credential is invalid')
    old_config['rcon_password']=password
    values=values_from_config(old_config)
    desired=read_legacy_env(project_env)
    # Do not silently apply unrelated pending settings during a format upgrade.
    drift=[key for key in CONFIG_KEYS if key in desired and key in values and desired[key]!=values[key]]
    require(not drift,'legacy .env differs from installed settings; resolve pending edits before migration: '+', '.join(drift))
    values.update(desired)
    directory=service_property(BACKEND,'WorkingDirectory')
    require(directory.startswith('/') and Path(directory).is_dir(),'existing Minecraft directory is unavailable')
    legacy_backend=(CONFIG_DIR/'backend.env').read_text().strip()
    key,separator,raw=legacy_backend.partition('=')
    require(separator and key.strip()=='MC_SERVER_ARGFILE','unexpected legacy backend.env format')
    parsed=shlex.split(raw,comments=False,posix=True)
    require(len(parsed)==1 and parsed[0].startswith('@'),'existing loader argument is invalid')
    require((Path(directory)/parsed[0][1:]).is_file(),'installed loader argument file is missing')
    values.update(MC_CONFIG=str(CANONICAL),MC_BINARY=str(BINARY),MC_SERVICE_USER=user,
                  MC_GATEWAY_SERVICE=GATEWAY,MC_RCON_PASSWORD=password,
                  MC_SERVER_DIRECTORY=directory,MC_JAVA_BINARY='/usr/bin/java',
                  MC_JVM_ARGFILE='@user_jvm_args.txt',MC_SERVER_ARGFILE=parsed[0])
    old_plan=ROOT/'.local/install.json'
    if old_plan.exists():
        backup=json.loads(old_plan.read_text()).get('backup_directory')
        if isinstance(backup,str) and backup.startswith('/'):
            values['MC_BACKUP_DIRECTORY']=backup
    values={**RUNTIME_DEFAULTS,**MANAGE_DEFAULTS,**values}
    env_data=serialize_env(values).encode()
    source=ROOT/'bin/mc-gateway'
    require(source.is_file(),'build the new Linux executable first')
    source_bytes=source.read_bytes()
    require(source_bytes.startswith(b'\x7fELF'),'build the new Linux executable first')
    new_version=run([str(source),'version'])
    require(new_version=='0.2.0','migration requires task build VERSION=0.2.0')
    unit_paths=[Path('/etc/systemd/system')/GATEWAY,Path('/etc/systemd/system')/BACKEND]
    units=[render_unit(GATEWAY,user,group),render_unit(BACKEND,user,group)]
    for path in unit_paths+[old_config_path,password_path,CONFIG_DIR/'backend.env',BINARY]:
        require(path.is_file() and not path.is_symlink(),'expected installation file is missing or a symlink')
    previous_binary=BINARY.with_name('mc-gateway.previous')
    if previous_binary.exists() or previous_binary.is_symlink():
        require(previous_binary.is_file() and not previous_binary.is_symlink(),
                'previous gateway executable is not a regular file; inspect before migration')
    installed_version=run([str(BINARY),'version'])
    validate_candidate(source_bytes,env_data,[GATEWAY,BACKEND],units,new_version)
    verify_process(BACKEND,before_pid,before_identity)
    verify_process(GATEWAY,gateway_pid,gateway_identity)
    if check_only:
        print(json.dumps({'status':'preflight-ok','version':new_version,
                          'installed_version_before':installed_version,'running_version_before':running_version,
                          'backend_pid':before_pid,'backend_start_time_ticks':before_identity,
                          'gateway_pid':gateway_pid,'gateway_start_time_ticks':gateway_identity,
                          'gateway_action':'one restart','minecraft_action':'preserve original process',
                          'config':str(CANONICAL),'config_directory_mode':'2750','config_mode':'0640',
                          'operator':owner.pw_name,'service_user':user,'service_group':group,
                          'persistent_changes':False}))
        return
    timestamp=datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    local=ROOT/'.local';local.mkdir(mode=0o700,exist_ok=True)
    archive=local/f'env-migration-{timestamp}.tar.gz'
    report_path=local/'env-migration.json'
    originals={p:p.read_bytes() for p in [project_env,BINARY,*unit_paths]}
    previous_metadata=previous_binary.stat() if previous_binary.exists() else None
    if previous_metadata is not None:
        originals[previous_binary]=previous_binary.read_bytes()
    directory_metadata=CONFIG_DIR.stat()
    archive_metadata={
        'installed_version_before':installed_version,'running_version_before':running_version,
        'running_gateway_pid':gateway_pid,'running_gateway_start_time_ticks':gateway_identity,
        'running_gateway_sha256':hashlib.sha256(running_executable).hexdigest(),
        'backend_pid_before':before_pid,'backend_start_time_ticks_before':before_identity,
        'config_directory_uid':directory_metadata.st_uid,'config_directory_gid':directory_metadata.st_gid,
        'config_directory_mode':oct(stat.S_IMODE(directory_metadata.st_mode)),
    }
    snapshot_archive(archive,[project_env,BINARY,previous_binary,*unit_paths,old_config_path,password_path,CONFIG_DIR/'backend.env',old_plan],
                     running_executable,archive_metadata)
    report={'version':new_version,'created_at':timestamp,'backup':str(archive),'backend_pid_before':before_pid,
            'backend_start_time_ticks_before':before_identity,'running_version_before':running_version,
            'installed_version_before':installed_version,'running_gateway_sha256':archive_metadata['running_gateway_sha256'],
            'status':'staging','phase':'install canonical environment'}
    activation_started=False

    def save_report():
        write_atomic(report_path,(json.dumps(report,indent=2)+'\n').encode(),0o600,owner.pw_uid,owner.pw_gid)

    save_report()
    try:
        verify_process(BACKEND,before_pid,before_identity)
        verify_process(GATEWAY,gateway_pid,gateway_identity)
        # Setgid allows an editor's atomic replacement to retain service-group
        # access without granting traversal of the operator's private home.
        os.chown(CONFIG_DIR,owner.pw_uid,account.pw_gid);os.chmod(CONFIG_DIR,0o2750)
        write_atomic(CANONICAL,env_data,0o640,owner.pw_uid,account.pw_gid)
        report['phase']='validate new executable and environment'
        staged=BINARY.parent/'.mc-gateway-migrate'
        require(not staged.exists(),'staging binary already exists; inspect earlier migration')
        try:
            write_atomic(staged,source_bytes,0o755)
            require(run([str(staged),'version'])==new_version,'staged executable version differs from validated candidate')
            run(['/usr/sbin/runuser','-u',user,'--',str(staged),'check','--config',str(CANONICAL)])
            write_atomic(BINARY.with_name('mc-gateway.previous'),originals[BINARY],0o755)
            os.replace(staged,BINARY)
        finally:
            staged.unlink(missing_ok=True)
        report['phase']='validate new service units'
        for path,data in zip(unit_paths,units):
            write_atomic(path,data,0o644)
        run(['/usr/bin/systemd-analyze','verify',*(str(p) for p in unit_paths)])
        project_env.unlink();project_env.symlink_to(CANONICAL)
        os.lchown(project_env,owner.pw_uid,owner.pw_gid)
        run(['/usr/bin/systemctl','daemon-reload'])
        report['phase']='restart gateway only';save_report()
        activation_started=True
        run(['/usr/bin/systemctl','restart',GATEWAY])
        report['phase']='verify running gateway and unchanged Java process'
        status=None
        deadline=time.monotonic()+45
        while time.monotonic()<deadline:
            remaining=deadline-time.monotonic()
            if remaining<=0:break
            candidate=gateway_status(user,values['MC_ADMIN_SOCKET'],timeout=min(5,remaining))
            if (candidate is not None and candidate.get('version')==new_version
                    and candidate.get('configuration',{}).get('sha256')==hashlib.sha256(env_data).hexdigest()
                    and lifecycle_healthy(candidate,before_identity is not None)):
                status=candidate;break
            time.sleep(0.25)
        require(status is not None,'new gateway did not establish healthy lifecycle/RCON state within 45 seconds; inspect task logs')
        require(status.get('configuration',{}).get('sha256')==hashlib.sha256(env_data).hexdigest(),
                'running configuration does not match the migrated .env')
        after_pid=service_property(BACKEND,'MainPID')
        after_identity=process_identity(after_pid)
        require(after_pid==before_pid and after_identity==before_identity,
                'original Minecraft process identity changed during migration; inspect before retiring old files')
        require(service_property(GATEWAY,'ActiveState')=='active','gateway is not active after restart')
        # Retire only old configuration inputs. Their historical contents remain
        # in the private archive; they can no longer drift as active settings.
        for path in (old_config_path,password_path,CONFIG_DIR/'backend.env',old_plan):
            path.unlink(missing_ok=True)
        report.update(status='complete',phase='complete',backend_pid_after=after_pid,
                      backend_start_time_ticks_after=after_identity,
                      gateway_pid=service_property(GATEWAY,'MainPID'),configuration_sha256=hashlib.sha256(env_data).hexdigest())
        save_report()
        print(json.dumps({'status':'complete','version':new_version,'gateway':'restarted',
                          'minecraft':'same process; not restarted','config':str(CANONICAL),'report':str(report_path)}))
    except Exception:
        report.update(status='failed')
        if not activation_started:
            # Restore installation metadata only. Never restore or touch a world.
            if project_env.is_symlink():project_env.unlink()
            write_atomic(project_env,originals[project_env],stat.S_IMODE(metadata.st_mode),metadata.st_uid,metadata.st_gid)
            for path in [BINARY,*unit_paths]:
                write_atomic(path,originals[path],0o755 if path==BINARY else 0o644)
            if previous_metadata is not None:
                write_atomic(previous_binary,originals[previous_binary],stat.S_IMODE(previous_metadata.st_mode),
                             previous_metadata.st_uid,previous_metadata.st_gid)
            else:
                previous_binary.unlink(missing_ok=True)
            CANONICAL.unlink(missing_ok=True)
            os.chown(CONFIG_DIR,directory_metadata.st_uid,directory_metadata.st_gid)
            os.chmod(CONFIG_DIR,stat.S_IMODE(directory_metadata.st_mode))
            run(['/usr/bin/systemctl','daemon-reload'])
            report['recovery']='pre-activation files restored; Minecraft untouched'
        else:
            report['recovery']='activation started; keep validated files and legacy inputs for inspection; Minecraft not signalled'
        save_report()
        raise RuntimeError('migration failed during '+report['phase']+'; inspect the private migration report') from None


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--check',action='store_true',help='inspect the complete migration plan without persistent writes or service actions')
    args=parser.parse_args()
    if os.geteuid()!=0:
        os.execv('/usr/bin/sudo',['sudo','-n',sys.executable,str(Path(__file__).resolve()),*sys.argv[1:]])
    migrate(check_only=args.check)


if __name__=='__main__':
    try:
        main()
    except Exception as exc:
        print('mc-gateway migrate: '+str(exc),file=sys.stderr)
        sys.exit(1)
