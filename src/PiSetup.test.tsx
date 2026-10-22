import { describe, expect, test } from "bun:test";
import { renderToStaticMarkup } from "react-dom/server";
import { PiSetup, PI_INSTALL_COMMAND, PI_INSTALL_URL } from "./PiSetup";

describe("Pi installation guidance", () => {
  test("offers a verified installer and retry when Pi is missing", () => {
    const html = renderToStaticMarkup(<PiSetup installation={{ path: "", version: "", status: "missing", detail: "未找到本机 Pi" }} detecting={false} onDetect={() => {}} onOpenInstall={() => {}} />);
    expect(html).toContain("先安装 Pi");
    expect(html).toContain("下载官方安装包");
    expect(html).toContain("重新检测");
    expect(html).toContain("复制安装命令");
    expect(html).toContain(PI_INSTALL_COMMAND);
    expect(PI_INSTALL_URL).toBe("https://github.com/earendil-works/pi/releases/tag/v1.4.0");
    expect(html).toContain("Node.js 22.19.0");
  });

  test("keeps version and execution failures actionable without hiding their cause", () => {
    for (const status of ["unsupported", "unavailable"] as const) {
      const html = renderToStaticMarkup(<PiSetup installation={{ path: "/custom/pi", version: "0.9.0", status, detail: "需要 Pi 1.4.0" }} detecting={true} onDetect={() => {}} onOpenInstall={() => {}} />);
      expect(html).toContain(status === "unsupported" ? "安装兼容的 Pi 版本" : "暂时无法运行 Pi");
      expect(html).toContain("需要 Pi 1.4.0");
      expect(html).toContain("正在检测…");
    }
  });

  test("shows no installation guidance for a supported Pi", () => {
    const html = renderToStaticMarkup(<PiSetup installation={{ path: "/usr/local/bin/pi", version: "1.4.0", status: "ready", detail: "" }} detecting={false} onDetect={() => {}} onOpenInstall={() => {}} />);
    expect(html).toBe("");
  });
});
