import type { Metadata } from "next";
import "./globals.css";
export const metadata: Metadata = {
  title: "Tunnel · 多站点访问网关",
  description: "连接分散的内网，从一个入口开始。",
};
export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="zh-CN">
      <body>{children}</body>
    </html>
  );
}
