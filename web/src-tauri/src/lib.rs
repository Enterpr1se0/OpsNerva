use serde::Deserialize;
use std::path::{Component, Path, PathBuf};
use std::process::Command;
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicBool, Ordering},
};
use std::time::Duration;
use tauri::{Manager, RunEvent, WindowEvent};
use tauri_plugin_shell::{ShellExt, process::CommandEvent};

mod desktop_window;

use desktop_window::{
    DesktopWindowState, TrayState, enter_lightweight_mode, open_developer_tools,
    setup_tray, show_main_window,
};

const READY_PREFIX: &str = "OPSNERVA_DESKTOP_READY=";

#[derive(Default)]
struct SidecarState(Mutex<Option<tauri_plugin_shell::process::CommandChild>>);

#[derive(Default)]
struct DesktopWorkspaceState(Mutex<Option<PathBuf>>);

#[derive(Debug, Deserialize, PartialEq)]
struct DesktopReady {
    url: String,
    #[serde(default)]
    workspace_root: String,
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    let app = tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, _, _| {
            show_main_window(app);
        }))
        .plugin(tauri_plugin_shell::init())
        .manage(SidecarState::default())
        .manage(DesktopWorkspaceState::default())
        .manage(DesktopWindowState::default())
        .manage(TrayState::default())
        .invoke_handler(tauri::generate_handler![
            enter_lightweight_mode,
            open_developer_tools,
            open_workspace_directory,
            open_external_url
        ])
        .on_window_event(|window, event| {
            if let WindowEvent::CloseRequested { api, .. } = event {
                let tray = window.state::<TrayState>();
                if !tray.exiting() {
                    api.prevent_close();
                    let _ = window.hide();
                }
            }
        })
        .setup(|app| {
            setup_tray(app)?;
            start_sidecar(app)
        })
        .build(tauri::generate_context!())
        .expect("failed to build OpsNerva desktop application");

    app.run(|handle, event| match event {
        RunEvent::ExitRequested { code, api, .. } => {
            let tray = handle.state::<TrayState>();
            if code.is_none() && !tray.exiting() {
                api.prevent_exit();
            }
        }
        RunEvent::Exit => {
            if let Some(child) = handle.state::<SidecarState>().0.lock().unwrap().take() {
                let _ = child.kill();
            }
        }
        _ => {}
    });
}

#[tauri::command]
fn open_workspace_directory(
    workspace_id: String,
    relative_path: String,
    state: tauri::State<'_, DesktopWorkspaceState>,
) -> Result<(), String> {
    let root = state
        .0
        .lock()
        .map_err(|_| "Workspace state is unavailable".to_string())?
        .clone()
        .ok_or_else(|| "Workspace directory is unavailable".to_string())?;
    let directory = resolve_workspace_directory(&root, &workspace_id, &relative_path)?;
    open_directory(&directory)
}

#[tauri::command]
fn open_external_url(url: String) -> Result<(), String> {
    let parsed = tauri::Url::parse(&url).map_err(|_| "Invalid URL".to_string())?;
    let loopback = parsed
        .host_str()
        .and_then(|host| host.parse::<std::net::IpAddr>().ok())
        .is_some_and(|address| address.is_loopback())
        || parsed.host_str() == Some("localhost");
    if parsed.scheme() != "https" && !(parsed.scheme() == "http" && loopback) {
        return Err("Only HTTPS and loopback HTTP URLs can be opened".into());
    }

    #[cfg(target_os = "windows")]
    let mut command = {
        let mut command = Command::new("rundll32.exe");
        command.arg("url.dll,FileProtocolHandler");
        command
    };
    #[cfg(target_os = "macos")]
    let mut command = Command::new("open");
    #[cfg(all(unix, not(target_os = "macos")))]
    let mut command = Command::new("xdg-open");
    #[cfg(not(any(target_os = "windows", target_os = "macos", unix)))]
    return Err("Opening URLs is unsupported on this platform".into());

    let mut child = command
        .arg(url)
        .spawn()
        .map_err(|error| format!("Open URL: {error}"))?;
    std::thread::spawn(move || {
        let _ = child.wait();
    });
    Ok(())
}

fn resolve_workspace_directory(
    workspace_root: &Path,
    workspace_id: &str,
    relative_path: &str,
) -> Result<PathBuf, String> {
    let workspace_id = workspace_id.trim();
    if workspace_id.is_empty()
        || workspace_id.len() > 64
        || workspace_id == "."
        || workspace_id == ".."
        || !workspace_id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'-'))
    {
        return Err("Invalid Workspace identifier".into());
    }
    let relative = if relative_path.is_empty() {
        Path::new(".")
    } else {
        Path::new(relative_path)
    };
    if relative.is_absolute()
        || relative
            .components()
            .any(|component| !matches!(component, Component::CurDir | Component::Normal(_)))
    {
        return Err("Invalid Workspace directory".into());
    }
    let workspace = workspace_root
        .join(workspace_id)
        .canonicalize()
        .map_err(|error| format!("Workspace directory is unavailable: {error}"))?;
    let target = workspace
        .join(relative)
        .canonicalize()
        .map_err(|error| format!("Workspace directory is unavailable: {error}"))?;
    if !target.starts_with(&workspace) {
        return Err("Workspace directory escapes its root".into());
    }
    if !target.is_dir() {
        return Err("Workspace path is not a directory".into());
    }
    Ok(target)
}

fn open_directory(directory: &Path) -> Result<(), String> {
    #[cfg(target_os = "windows")]
    let mut command = Command::new("explorer.exe");
    #[cfg(target_os = "macos")]
    let mut command = Command::new("open");
    #[cfg(all(unix, not(target_os = "macos")))]
    let mut command = Command::new("xdg-open");
    #[cfg(not(any(target_os = "windows", target_os = "macos", unix)))]
    return Err("Opening a file manager is unsupported on this platform".into());

    let mut child = command
        .arg(file_manager_path(directory))
        .spawn()
        .map_err(|error| format!("Open file manager: {error}"))?;
    std::thread::spawn(move || {
        let _ = child.wait();
    });
    Ok(())
}

#[cfg(target_os = "windows")]
fn file_manager_path(path: &Path) -> PathBuf {
    let value = path.to_string_lossy();
    if let Some(value) = value.strip_prefix(r"\\?\UNC\") {
        return PathBuf::from(format!(r"\\{value}"));
    }
    if let Some(value) = value.strip_prefix(r"\\?\") {
        return PathBuf::from(value);
    }
    path.to_path_buf()
}

#[cfg(not(target_os = "windows"))]
fn file_manager_path(path: &Path) -> PathBuf {
    path.to_path_buf()
}

fn start_sidecar(app: &mut tauri::App) -> Result<(), Box<dyn std::error::Error>> {
    let executable = std::env::current_exe()?;
    let install_dir = executable.parent().ok_or_else(|| {
        std::io::Error::new(
            std::io::ErrorKind::NotFound,
            "desktop executable has no installation directory",
        )
    })?;

    let command = app
        .shell()
        .sidecar("opsnerva")?
        .env("OPSNERVA_HOME", install_dir)
        .env("OPSNERVA_DESKTOP", "true")
        .env("OPSNERVA_LISTEN", "127.0.0.1:0")
        .current_dir(install_dir);
    let (mut events, child) = command.spawn()?;
    app.state::<SidecarState>().0.lock().unwrap().replace(child);

    let handle = app.handle().clone();
    let ready = Arc::new(AtomicBool::new(false));
    let event_ready = Arc::clone(&ready);
    tauri::async_runtime::spawn(async move {
        while let Some(event) = events.recv().await {
            match event {
                CommandEvent::Stdout(bytes) => {
                    let line = String::from_utf8_lossy(&bytes);
                    match parse_ready(&line) {
                        Ok(Some(response)) => {
                            event_ready.store(true, Ordering::Release);
                            open_application(&handle, response);
                        }
                        Ok(None) => {}
                        Err(error) => show_startup_error(
                            &handle,
                            format!("Invalid backend startup response: {error}"),
                        ),
                    }
                }
                CommandEvent::Error(error) => show_startup_error(&handle, error),
                CommandEvent::Terminated(status) if !event_ready.load(Ordering::Acquire) => {
                    show_startup_error(
                        &handle,
                        format!(
                            "Backend stopped before the application was ready ({:?}).",
                            status.code
                        ),
                    );
                }
                _ => {}
            }
        }
    });

    let timeout_handle = app.handle().clone();
    std::thread::spawn(move || {
        std::thread::sleep(Duration::from_secs(90));
        if !ready.load(Ordering::Acquire) {
            if let Some(child) = timeout_handle
                .state::<SidecarState>()
                .0
                .lock()
                .unwrap()
                .take()
            {
                let _ = child.kill();
            }
            show_startup_error(
                &timeout_handle,
                "Backend startup timed out after 90 seconds.".into(),
            );
        }
    });
    Ok(())
}

fn open_application(app: &tauri::AppHandle, ready: DesktopReady) {
    let destination = match application_url(&ready) {
        Ok(url) => url,
        Err(error) => {
            show_startup_error(app, format!("Invalid backend URL: {error}"));
            return;
        }
    };
    if !ready.workspace_root.trim().is_empty() {
        app.state::<DesktopWorkspaceState>()
            .0
            .lock()
            .unwrap()
            .replace(PathBuf::from(&ready.workspace_root));
    }
    app.state::<DesktopWindowState>()
        .set_url(destination.clone());
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.navigate(destination);
    }
}

fn parse_ready(line: &str) -> Result<Option<DesktopReady>, serde_json::Error> {
    let Some(payload) = line.trim().strip_prefix(READY_PREFIX) else {
        return Ok(None);
    };
    serde_json::from_str(payload).map(Some)
}

fn application_url(ready: &DesktopReady) -> Result<tauri::Url, String> {
    let destination = ready
        .url
        .parse::<tauri::Url>()
        .map_err(|error| error.to_string())?;
    if destination.scheme() != "http"
        || destination.host_str() != Some("127.0.0.1")
        || destination.port().is_none()
        || !destination.username().is_empty()
        || destination.password().is_some()
    {
        return Err("backend URL must use an explicit 127.0.0.1 HTTP port".into());
    }
    Ok(destination)
}

fn show_startup_error(app: &tauri::AppHandle, message: String) {
    if let Some(window) = app.get_webview_window("main") {
        let localized = format!("本地服务启动失败 / Local service failed to start: {message}");
        let argument =
            serde_json::to_string(&localized).unwrap_or_else(|_| "\"Startup failed\"".into());
        let _ = window.eval(&format!("window.desktopStartupError({argument})"));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_desktop_ready_event() {
        assert_eq!(parse_ready("ordinary log line").unwrap(), None);
        let ready = parse_ready(
            r#"OPSNERVA_DESKTOP_READY={"url":"http://127.0.0.1:49152","workspace_root":"/tmp/opsnerva/workspace"}"#,
        )
        .unwrap()
        .unwrap();
        assert_eq!(
            ready,
            DesktopReady {
                url: "http://127.0.0.1:49152".into(),
                workspace_root: "/tmp/opsnerva/workspace".into(),
            }
        );
    }

    #[test]
    fn rejects_workspace_directory_traversal_before_filesystem_access() {
        assert!(resolve_workspace_directory(Path::new("/unused"), "default", "../secret").is_err());
        assert!(resolve_workspace_directory(Path::new("/unused"), "../default", ".").is_err());
    }

    #[test]
    fn uses_backend_url_without_credentials() {
        let ready = DesktopReady {
            url: "http://127.0.0.1:49152".into(),
            workspace_root: String::new(),
        };
        let url = application_url(&ready).unwrap();
        assert_eq!(url.as_str(), "http://127.0.0.1:49152/");
    }

    #[test]
    fn rejects_non_loopback_backend_url() {
        let ready = DesktopReady {
            url: "https://example.com/".into(),
            workspace_root: String::new(),
        };
        assert!(application_url(&ready).is_err());
    }
}
