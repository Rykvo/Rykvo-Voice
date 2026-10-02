"""RVU1: signed ciphertext; plaintext is usable only after GCM finalization."""
import hashlib
import hmac
import os
from pathlib import Path
import stat
import struct
import tempfile

from cryptography.exceptions import InvalidSignature, InvalidTag
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey, Ed25519PublicKey
from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes

HEADER = struct.Struct(">8s12sQ")
MAGIC = b"RYKVOU15"
SEAL_SIZE = 96
MAX_PLAIN = 129 * 1024 * 1024
MAX_PACKAGE = MAX_PLAIN + HEADER.size + 16 + SEAL_SIZE
DOMAIN = b"Rykvo Voice encrypted update v1\x00"
PUBLIC_KEY = Path(__file__).with_name("update-signing.pub")
DECRYPT_KEY = Path("/etc/rykvo-voice/update.key")
INSTALL_MAGIC = b"RYKVOK15"
INSTALL_DOMAIN = b"Rykvo Voice install key v1\x00"
INSTALL_SIZE = 104


def public_key():
    return Ed25519PublicKey.from_public_bytes(bytes.fromhex(PUBLIC_KEY.read_text().strip()))


def read_key(path=None):
    path = Path(DECRYPT_KEY if path is None else path)
    try:
        flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
        fd = os.open(path, flags)
        with os.fdopen(fd, "rb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_size > 128:
                raise ValueError()
            if os.name == "posix" and (info.st_uid != 0 or info.st_mode & 0o077):
                raise ValueError()
            value = bytes.fromhex(stream.read(128).decode().strip())
        if len(value) != 32:
            raise ValueError()
        return value
    except (OSError, ValueError, UnicodeError):
        raise ValueError("UPDATE_KEY_UNAVAILABLE") from None


def create_install_key(key, signing_key):
    """Signed provisioning material; deliberately recoverable by installers."""
    if len(key) != 32:
        raise ValueError("INVALID_INSTALL_KEY")
    payload = INSTALL_MAGIC + key
    return payload + Ed25519PrivateKey.from_private_bytes(signing_key).sign(INSTALL_DOMAIN + payload)


def inspect_install_key(data, trusted=None):
    try:
        if len(data) != INSTALL_SIZE or data[:8] != INSTALL_MAGIC:
            raise ValueError()
        (trusted or public_key()).verify(data[40:], INSTALL_DOMAIN + data[:40])
        return data[8:40]
    except (InvalidSignature, ValueError, OSError):
        raise ValueError("INSTALL_SIGNATURE_INVALID") from None


def provision_install_key(data, path=None, trusted=None):
    key = inspect_install_key(data, trusted)
    path = Path(DECRYPT_KEY if path is None else path)
    if path.exists() or path.is_symlink():
        if not hmac.compare_digest(read_key(path), key):
            raise ValueError("INSTALL_KEY_CONFLICT")
        return
    directory = path.parent
    if not directory.is_absolute() or directory.resolve() != directory:
        raise ValueError("UPDATE_KEY_UNAVAILABLE")
    directory.mkdir(mode=0o700, exist_ok=True)
    if os.name == "posix" and directory.stat().st_uid != os.geteuid():
        raise ValueError("UPDATE_KEY_UNAVAILABLE")
    directory.chmod(0o700)
    fd, temporary = tempfile.mkstemp(prefix=".update-key-", dir=directory)
    try:
        with os.fdopen(fd, "w") as stream:
            stream.write(key.hex() + "\n")
            stream.flush(); os.fsync(stream.fileno())
        try:
            # Atomic creation without overwriting an existing or concurrent key.
            os.link(temporary, path)
        except FileExistsError:
            if not hmac.compare_digest(read_key(path), key):
                raise ValueError("INSTALL_KEY_CONFLICT") from None
    finally:
        Path(temporary).unlink(missing_ok=True)


def inspect(header, seal, size, trusted=None):
    try:
        if len(header) != HEADER.size or len(seal) != SEAL_SIZE or type(size) is not int:
            raise ValueError()
        magic, nonce, plain_size = HEADER.unpack(header)
        if magic != MAGIC or not 0 < plain_size <= MAX_PLAIN or size != plain_size + HEADER.size + 16 + SEAL_SIZE:
            raise ValueError()
        (trusted or public_key()).verify(seal[32:], DOMAIN + header + seal[:32])
        return nonce, plain_size
    except (InvalidSignature, ValueError, OSError):
        raise ValueError("UPDATE_SIGNATURE_INVALID") from None


def encrypt(source, destination, key, signing_key):
    source, destination = Path(source), Path(destination)
    size = source.stat().st_size
    if not 0 < size <= MAX_PLAIN or len(key) != 32:
        raise ValueError("INVALID_UPDATE_PACKAGE")
    private = Ed25519PrivateKey.from_private_bytes(signing_key)
    header = HEADER.pack(MAGIC, os.urandom(12), size)
    encryptor = Cipher(algorithms.AES(key), modes.GCM(HEADER.unpack(header)[1])).encryptor()
    encryptor.authenticate_additional_data(header)
    temporary = destination.with_suffix(".tmp")
    digest = hashlib.sha256(header)
    try:
        with source.open("rb") as inp, temporary.open("xb") as out:
            out.write(header)
            while chunk := inp.read(1024 * 1024):
                value = encryptor.update(chunk)
                digest.update(value); out.write(value)
            value = encryptor.finalize() + encryptor.tag
            digest.update(value); out.write(value)
            checksum = digest.digest()
            out.write(checksum + private.sign(DOMAIN + header + checksum))
            out.flush(); os.fsync(out.fileno())
        temporary.replace(destination)
    finally:
        temporary.unlink(missing_ok=True)


def decrypt(source, destination, key=None, trusted=None):
    source, destination = Path(source), Path(destination)
    temporary = destination.with_suffix(".decrypting")
    try:
        with source.open("rb") as inp:
            size = os.fstat(inp.fileno()).st_size
            if size > MAX_PACKAGE or size < HEADER.size + 16 + SEAL_SIZE + 1:
                raise ValueError("UPDATE_SIGNATURE_INVALID")
            header = inp.read(HEADER.size)
            inp.seek(-SEAL_SIZE, os.SEEK_END); seal = inp.read(SEAL_SIZE)
            nonce, plain_size = inspect(header, seal, size, trusted)
            # Authenticate the complete ciphertext before producing any plaintext.
            inp.seek(0); digest = hashlib.sha256(); left = size - SEAL_SIZE
            while left:
                value = inp.read(min(left, 1024 * 1024))
                if not value: raise ValueError("INVALID_UPDATE_PACKAGE")
                digest.update(value); left -= len(value)
            if digest.digest() != seal[:32]:
                raise ValueError("UPDATE_SIGNATURE_INVALID")
            inp.seek(HEADER.size + plain_size); tag = inp.read(16)
            decryptor = Cipher(algorithms.AES(key if key is not None else read_key()), modes.GCM(nonce, tag)).decryptor()
            decryptor.authenticate_additional_data(header)
            inp.seek(HEADER.size); left = plain_size
            with temporary.open("xb") as out:
                if os.name == "posix": os.fchmod(out.fileno(), 0o600)
                while left:
                    value = inp.read(min(left, 1024 * 1024))
                    if not value: raise ValueError("INVALID_UPDATE_PACKAGE")
                    out.write(decryptor.update(value)); left -= len(value)
                out.write(decryptor.finalize())
                out.flush(); os.fsync(out.fileno())
        temporary.replace(destination)
    except InvalidTag:
        raise ValueError("UPDATE_DECRYPT_FAILED") from None
    finally:
        temporary.unlink(missing_ok=True)
