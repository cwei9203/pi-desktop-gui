import { Check, Copy, ExternalLink, RefreshCw, Terminal } from "lucide-react";
import { useState } from "react";
import type { PiInstallation } from "./mygo";

export const PI_INSTALL_COMMAND = "npm install -g --ignore-scripts @earendil-works/pi-coding-agent@1.4.0";
export const PI_INSTALL_URL = "https://github.com/earendil-works/pi/releases/tag/v1.4.0";

export function PiSetup({ installation, detecting, onDetect, onOpenInstall }: { installation: PiInstallation; detecting: boolean; onDetect: () => void; onOpenInstall: () => void }) {
  const [copied, setCopied] = useState(false);
  const [copyFailed, setCopyFailed] = useState(false);
  if (installation.status === "ready") return null;
  const title = installation.status === "missing" ? "先安装 Pi" : installation.status === "unsupported" ? "安装兼容的 Pi 版本" : "暂时无法运行 Pi";
  return <section className="pi-setup" aria-labelledby="pi-setup-title">
    <span className="setup-icon"><Terminal size={20} /></span>
    <h1 id="pi-setup-title">{title}</h1>
    <p>Pi 工作台使用本机 Pi 执行任务。安装完成后，你的模型配置和会话也会留在本机。</p>
    {installation.detail && <p className="setup-detail">{installation.detail}</p>}
    <div className="setup-actions"><button type="button" className="primary-button" onClick={onOpenInstall}>下载官方安装包<ExternalLink size={13} /></button><button type="button" className="subtle-button" disabled={detecting} onClick={onDetect}><RefreshCw size={14} />{detecting ? "正在检测…" : "重新检测"}</button></div>
    <div className="install-alternative"><span>也可使用 npm 安装</span><small>需要 Node.js 22.19.0 或更新版本</small></div>
    <div className="install-command"><code>{PI_INSTALL_COMMAND}</code><button className="icon-button" type="button" aria-label={copied ? "已复制安装命令" : "复制安装命令"} onClick={async () => {
      try { await navigator.clipboard.writeText(PI_INSTALL_COMMAND); setCopied(true); setCopyFailed(false); }
      catch { setCopyFailed(true); }
    }}>{copied ? <Check size={16} /> : <Copy size={16} />}</button></div>
    {copyFailed && <p className="message-error">复制失败，请选择上方命令手动复制。</p>}
    <p className="setup-footnote">在终端完成安装与登录，再回到这里继续。</p>
  </section>;
}
