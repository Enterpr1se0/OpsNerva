use std::sync::{
    Mutex,
    atomic::{AtomicBool, Ordering},
};
use tauri::{
    AppHandle, Manager, WebviewUrl, WebviewWindowBuilder,
    menu::{MenuBuilder, MenuItem},
    tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent},
};

#[derive(Default)]
pub(crate) struct DesktopWindowState {
    url: Mutex<Option<tauri::Url>>,
    opening: AtomicBool,
}

impl DesktopWindowState {
    pub(crate) fn set_url(&self, url: tauri::Url) {
        self.url.lock().unwrap().replace(url);
    }
}

#[derive(Default)]
pub(crate) struct TrayState {
    exiting: AtomicBool,
    lightweight_item: Mutex<Option<MenuItem<tauri::Wry>>>,
}

impl TrayState {
    pub(crate) fn exiting(&self) -> bool {
        self.exiting.load(Ordering::Acquire)
    }

    fn begin_exit(&self) {
        self.exiting.store(true, Ordering::Release);
    }

    fn set_webview_available(&self, available: bool) -> Result<(), String> {
        let item = self
            .lightweight_item
            .lock()
            .map_err(|_| "tray menu state is unavailable".to_string())?;
        if let Some(item) = item.as_ref() {
            item.set_enabled(available)
                .map_err(|error| error.to_string())?;
        }
        Ok(())
    }

    fn set_lightweight_item(&self, item: MenuItem<tauri::Wry>) {
        self.lightweight_item.lock().unwrap().replace(item);
    }
}

#[tauri::command]
pub(crate) fn enter_lightweight_mode(app: AppHandle) -> Result<(), String> {
    destroy_main_webview(&app)
}

fn destroy_main_webview(app: &AppHandle) -> Result<(), String> {
    let tray = app.state::<TrayState>();
    let window = app
        .get_webview_window("main")
        .ok_or_else(|| "main window is unavailable".to_string())?;
    tray.set_webview_available(false)?;
    if let Err(error) = window.destroy() {
        let _ = tray.set_webview_available(true);
        return Err(error.to_string());
    }
    Ok(())
}

#[tauri::command]
pub(crate) fn open_developer_tools(app: AppHandle) -> Result<(), String> {
    let window = app
        .get_webview_window("main")
        .ok_or_else(|| "main window is unavailable".to_string())?;
    window.open_devtools();
    Ok(())
}

pub(crate) fn show_main_window(app: &AppHandle) {
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.show();
        let _ = window.unminimize();
        let _ = window.set_focus();
        let _ = app.state::<TrayState>().set_webview_available(true);
        return;
    }
    let state = app.state::<DesktopWindowState>();
    if state
        .opening
        .compare_exchange(false, true, Ordering::AcqRel, Ordering::Acquire)
        .is_err()
    {
        return;
    }
    let handle = app.clone();
    std::thread::spawn(move || {
        if let Err(error) = recreate_main_window(&handle) {
            eprintln!("failed to recreate OpsNerva window: {error}");
        }
        handle
            .state::<DesktopWindowState>()
            .opening
            .store(false, Ordering::Release);
    });
}

fn recreate_main_window(app: &AppHandle) -> Result<(), String> {
    if let Some(window) = app.get_webview_window("main") {
        window.show().map_err(|error| error.to_string())?;
        let _ = window.unminimize();
        let _ = window.set_focus();
        app.state::<TrayState>().set_webview_available(true)?;
        return Ok(());
    }
    let url = app
        .state::<DesktopWindowState>()
        .url
        .lock()
        .map_err(|_| "desktop window state is unavailable".to_string())?
        .clone()
        .ok_or_else(|| "backend URL is unavailable".to_string())?;
    let mut config = app
        .config()
        .app
        .windows
        .iter()
        .find(|config| config.label == "main")
        .cloned()
        .ok_or_else(|| "main window configuration is unavailable".to_string())?;
    config.url = WebviewUrl::External(url);
    config.visible = true;
    let window = WebviewWindowBuilder::from_config(app, &config)
        .map_err(|error| error.to_string())?
        .build()
        .map_err(|error| error.to_string())?;
    let _ = window.unminimize();
    let _ = window.set_focus();
    app.state::<TrayState>().set_webview_available(true)?;
    Ok(())
}

pub(crate) fn setup_tray(app: &mut tauri::App) -> Result<(), Box<dyn std::error::Error>> {
    let lightweight = MenuItem::with_id(
        app,
        "lightweight",
        "进入轻量模式",
        app.get_webview_window("main").is_some(),
        None::<&str>,
    )?;
    let menu = MenuBuilder::new(app)
        .text("open", "打开 OpsNerva")
        .item(&lightweight)
        .separator()
        .text("quit", "退出")
        .build()?;
    app.state::<TrayState>()
        .set_lightweight_item(lightweight);
    let mut tray = TrayIconBuilder::with_id("opsnerva")
        .menu(&menu)
        .tooltip("OpsNerva")
        .show_menu_on_left_click(false)
        .on_menu_event(|app, event| match event.id().as_ref() {
            "open" => show_main_window(app),
            "lightweight" => {
                if let Err(error) = destroy_main_webview(app) {
                    eprintln!("failed to enter lightweight mode: {error}");
                }
            }
            "quit" => {
                app.state::<TrayState>().begin_exit();
                app.exit(0);
            }
            _ => {}
        })
        .on_tray_icon_event(|tray, event| {
            if matches!(
                event,
                TrayIconEvent::Click {
                    button: MouseButton::Left,
                    button_state: MouseButtonState::Up,
                    ..
                }
            ) {
                show_main_window(tray.app_handle());
            }
        });
    if let Some(icon) = app.default_window_icon() {
        tray = tray.icon(icon.clone());
    }
    tray.build(app)?;
    Ok(())
}
