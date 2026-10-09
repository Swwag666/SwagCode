/*! DPAPI-обёртка для ключа провайдера (этап B-7, только Windows).

`CryptProtectData` шифрует байты под текущей учётной записью Windows:
blob, украденный с диска, бесполезен без логина этого пользователя.
Это «опционально» из плана: env-ключ из `.env` остаётся рабочим путём,
а DPAPI — способ держать ключ в базе prefs, не в тексте файла.

Хранение — hex-строка (база текстовая, base64-крейт не тянем ради одной
функции). На других платформах модуль честно отказывает: кроссплатформенный
keyring — отдельная задача, в план B-7 не входит.
*/

/// Зашифровать строку в hex-blob DPAPI.
#[cfg(windows)]
pub fn protect_hex(plain: &str) -> Result<String, String> {
    use windows_sys::Win32::Foundation::LocalFree;
    use windows_sys::Win32::Security::Cryptography::{CryptProtectData, CRYPTPROTECT_UI_FORBIDDEN};

    let data = plain.as_bytes();
    let in_blob = windows_sys::Win32::Security::Cryptography::CRYPT_INTEGER_BLOB {
        cbData: data.len() as u32,
        pbData: data.as_ptr() as *mut u8,
    };
    let mut out_blob = windows_sys::Win32::Security::Cryptography::CRYPT_INTEGER_BLOB {
        cbData: 0,
        pbData: std::ptr::null_mut(),
    };
    // UI_FORBIDDEN: фоновая служба не может показать диалог, а зависший
    // ход из-за невидимого промпта — худший исход.
    let ok = unsafe {
        CryptProtectData(
            &in_blob,
            std::ptr::null(),
            std::ptr::null(),
            std::ptr::null(),
            std::ptr::null_mut(),
            CRYPTPROTECT_UI_FORBIDDEN,
            &mut out_blob,
        )
    };
    if ok == 0 {
        return Err("CryptProtectData отказал".into());
    }
    let bytes =
        unsafe { std::slice::from_raw_parts(out_blob.pbData, out_blob.cbData as usize) }.to_vec();
    unsafe {
        LocalFree(out_blob.pbData as *mut _);
    }
    Ok(to_hex(&bytes))
}

/// Расшифровать hex-blob DPAPI обратно в строку.
#[cfg(windows)]
pub fn unprotect_hex(hex: &str) -> Result<String, String> {
    use windows_sys::Win32::Foundation::LocalFree;
    use windows_sys::Win32::Security::Cryptography::{
        CryptUnprotectData, CRYPTPROTECT_UI_FORBIDDEN,
    };

    let bytes = from_hex(hex)?;
    let in_blob = windows_sys::Win32::Security::Cryptography::CRYPT_INTEGER_BLOB {
        cbData: bytes.len() as u32,
        pbData: bytes.as_ptr() as *mut u8,
    };
    let mut out_blob = windows_sys::Win32::Security::Cryptography::CRYPT_INTEGER_BLOB {
        cbData: 0,
        pbData: std::ptr::null_mut(),
    };
    let ok = unsafe {
        CryptUnprotectData(
            &in_blob,
            std::ptr::null_mut(),
            std::ptr::null(),
            std::ptr::null(),
            std::ptr::null_mut(),
            CRYPTPROTECT_UI_FORBIDDEN,
            &mut out_blob,
        )
    };
    if ok == 0 {
        return Err("CryptUnprotectData отказал (blob не этой учётной записи?)".into());
    }
    let plain =
        unsafe { std::slice::from_raw_parts(out_blob.pbData, out_blob.cbData as usize) }.to_vec();
    unsafe {
        LocalFree(out_blob.pbData as *mut _);
    }
    String::from_utf8(plain).map_err(|_| "расшифрованные байты не UTF-8".into())
}

/// На других платформах DPAPI нет: честный отказ, env-ключ работает как раньше.
#[cfg(not(windows))]
pub fn protect_hex(_plain: &str) -> Result<String, String> {
    Err("DPAPI доступен только на Windows; используйте .env".into())
}

#[cfg(not(windows))]
pub fn unprotect_hex(_hex: &str) -> Result<String, String> {
    Err("DPAPI доступен только на Windows; используйте .env".into())
}

/// Hex-кодек: чистые функции, тестируются на любой платформе.
pub fn to_hex(bytes: &[u8]) -> String {
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push(char::from_digit((b >> 4) as u32, 16).unwrap());
        s.push(char::from_digit((b & 0xf) as u32, 16).unwrap());
    }
    s
}

pub fn from_hex(s: &str) -> Result<Vec<u8>, String> {
    if s.len() % 2 != 0 {
        return Err("hex нечётной длины".into());
    }
    let bytes = s.as_bytes();
    let mut out = Vec::with_capacity(bytes.len() / 2);
    for pair in bytes.chunks(2) {
        let hi = (pair[0] as char).to_digit(16).ok_or("не hex-символ")?;
        let lo = (pair[1] as char).to_digit(16).ok_or("не hex-символ")?;
        out.push((hi * 16 + lo) as u8);
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hex_roundtrip() {
        let data: Vec<u8> = (0..=255).collect();
        assert_eq!(from_hex(&to_hex(&data)).unwrap(), data);
        assert_eq!(to_hex(&[0x0f, 0xa5]), "0fa5");
        assert!(from_hex("0").is_err());
        assert!(from_hex("zz").is_err());
    }

    #[cfg(windows)]
    #[test]
    fn dpapi_roundtrip_protects_key() {
        let key = "sk-test-ключ-12345";
        let blob = protect_hex(key).expect("DPAPI доступен на Windows");
        assert!(!blob.contains(key), "blob не должен содержать plaintext");
        assert_eq!(unprotect_hex(&blob).unwrap(), key);
        // Порча blob — ошибка, не паника и не мусор.
        let mut broken = blob.clone();
        broken.replace_range(0..2, "ff");
        let res = unprotect_hex(&broken);
        assert!(res.is_err() || res.unwrap() != key);
    }
}
