// Предотвращает появление окна консоли в релизной сборке на Windows.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    swagcod_app_lib::run()
}
