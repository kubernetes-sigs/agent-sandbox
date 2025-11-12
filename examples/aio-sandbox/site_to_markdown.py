import asyncio
import base64
from playwright.async_api import async_playwright
from agent_sandbox import Sandbox

async def site_to_markdown():
    # Initialize sandbox client
    c = Sandbox(base_url="http://localhost:8080")
    home_dir = c.sandbox.get_context().home_dir

    # Browser: Automation to download HTML
    async with async_playwright() as p:
        browser_info = c.browser.get_info().data
        page = await (await p.chromium.connect_over_cdp(browser_info.cdp_url)).new_page()
        await page.goto("https://example.com", wait_until="networkidle")
        html = await page.content()
        screenshot_b64 = base64.b64encode(await page.screenshot()).decode('utf-8')

    # Jupyter: Convert HTML to markdown in sandbox
    c.jupyter.execute_code(code=f"""
from markdownify import markdownify
html = '''{html}'''
screenshot_b64 = "{screenshot_b64}"

md = f"{{markdownify(html)}}\\n\\n![Screenshot](data:image/png;base64,{{screenshot_b64}})"
with open('{home_dir}/site.md', 'w') as f:
    f.write(md)
print("Done!")
""")

    # Shell: List files in sandbox
    list_result = c.shell.exec_command(command=f"ls -lh {home_dir}")
    print(f"Files in sandbox: {list_result.data.output}")

    # File: Read the generated markdown
    return c.file.read_file(file=f"{home_dir}/site.md").data.content

if __name__ == "__main__":
    result = asyncio.run(site_to_markdown())
    print(f"Markdown saved successfully!")