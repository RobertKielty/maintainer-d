const { Given, When, Then } = require("@cucumber/cucumber");
const { expect } = require("@playwright/test");

const noDotProjectRepoName = "Project Beacon";

const resolveProjectId = async (world, name) => {
  const response = await world.page.request.get(
    `${world.bffBaseUrl}/api/search?query=${encodeURIComponent(name)}`
  );
  if (!response.ok()) {
    throw new Error(`Failed to search project: ${response.status()}`);
  }
  const data = await response.json();
  const project = (data.projects || []).find((item) => item.name === name);
  if (!project) {
    throw new Error(`Project ${name} not found in search results.`);
  }
  return project.id;
};

Given("a project exists without a .project repo", function () {
  this.projectName = noDotProjectRepoName;
});

When("I open the DOT-PROJECT ROLL CALL route for that project", async function () {
  this.projectId = await resolveProjectId(this, this.projectName);
  await this.context.grantPermissions(["clipboard-read", "clipboard-write"], {
    origin: this.baseUrl,
  });
  await this.page.goto(`${this.baseUrl}/projects/${this.projectId}/dot-project`, {
    waitUntil: "domcontentloaded",
  });
  await expect(this.page.getByRole("heading", { name: this.projectName })).toBeVisible({
    timeout: 15000,
  });
});

When("I click {string} on the generate MAINTAINERS.yaml panel", async function (buttonName) {
  const contentColumn = this.page.locator('[class*="contentColumn"]');
  await contentColumn.getByRole("button", { name: buttonName, exact: true }).click();
});

Then("the dot-project roll call shows a generate MAINTAINERS.yaml panel", async function () {
  const contentColumn = this.page.locator('[class*="contentColumn"]');
  await expect(contentColumn.getByText("Generate MAINTAINERS.yaml", { exact: true })).toBeVisible({
    timeout: 15000,
  });
  await expect(
    contentColumn.getByText("This project does not have a .project repo yet.", { exact: false })
  ).toBeVisible({ timeout: 15000 });
});

Then("the dot-project roll call does not show a generate MAINTAINERS.yaml panel", async function () {
  const contentColumn = this.page.locator('[class*="contentColumn"]');
  await expect(contentColumn.getByText("Generate MAINTAINERS.yaml", { exact: true })).toHaveCount(0);
});

Then("the generated MAINTAINERS.yaml lists the project's active maintainers", async function () {
  const contentColumn = this.page.locator('[class*="contentColumn"]');
  const generatedBlock = contentColumn.locator('[class*="dotProjectGeneratedYamlBlock"]');
  await expect(generatedBlock).toContainText('name: "maintainers"');
  await expect(generatedBlock).toContainText("antonio-example");
  await expect(generatedBlock).toContainText("diego-placeholder");
});

Then("the system clipboard contains the generated MAINTAINERS.yaml", async function () {
  const clipboardText = await this.page.evaluate(() => navigator.clipboard.readText());
  expect(clipboardText).toContain('name: "maintainers"');
  expect(clipboardText).toContain("antonio-example");
  expect(clipboardText).toContain("diego-placeholder");
});

Then("a {string} confirmation is shown", async function (message) {
  const contentColumn = this.page.locator('[class*="contentColumn"]');
  await expect(contentColumn.getByText(message, { exact: true })).toBeVisible({ timeout: 15000 });
});
