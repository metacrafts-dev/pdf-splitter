(() => {
  "use strict";

  const i18n = window.PDF_SPLITTER_I18N || {};
  const csrfMeta = document.querySelector('meta[name="csrf-token"]');
  const csrfToken = csrfMeta ? csrfMeta.getAttribute("content") : "";

  let currentBook = null;
  let currentFile = null;
  let currentSessionId = "";
  let splitPoints = [];
  let lastSplitData = null;
  let currentXhr = null;
  let isStandaloneMode = false;
  let cachedServerConfig = null;
  let sseSource = null;

  const dropzone = document.getElementById("dropzone");
  const fileInput = document.getElementById("fileInput");
  const uploadProgress = document.getElementById("uploadProgress");
  const progressFileName = document.getElementById("progressFileName");
  const progressStatusText = document.getElementById("progressStatusText");
  const progressPercentBadge = document.getElementById("progressPercentBadge");
  const progressBarFill = document.getElementById("progressBarFill");
  const progressBytesText = document.getElementById("progressBytesText");
  const progressStorageText = document.getElementById("progressStorageText");

  const bookInfo = document.getElementById("bookInfo");
  const bookName = document.getElementById("bookName");
  const bookSize = document.getElementById("bookSize");
  const bookPagesBadge = document.getElementById("bookPagesBadge");
  const clearBookBtn = document.getElementById("clearBookBtn");

  const splitSection = document.getElementById("splitSection");
  const splitInput = document.getElementById("splitInput");
  const addPointBtn = document.getElementById("addPointBtn");
  const presetExampleBtn = document.getElementById("presetExampleBtn");
  const clearPointsBtn = document.getElementById("clearPointsBtn");
  const chipsContainer = document.getElementById("chipsContainer");
  const partsList = document.getElementById("partsList");
  const splitNowBtn = document.getElementById("splitNowBtn");
  const splitSpinner = document.getElementById("splitSpinner");

  const resultsCard = document.getElementById("resultsCard");
  const statusBox = document.getElementById("statusBox");
  const statusText = document.getElementById("statusText");
  const downloadZipBtn = document.getElementById("downloadZipBtn");
  const openFolderBtn = document.getElementById("openFolderBtn");
  const resultsTableBody = document.getElementById("resultsTableBody");
  const retentionText = document.getElementById("retentionText");

  const langEnBtn = document.getElementById("langEnBtn");
  const langArBtn = document.getElementById("langArBtn");
  const openAppModalBtn = document.getElementById("openAppModalBtn");
  const closeAppModalBtn = document.getElementById("closeAppModalBtn");
  const modalDismissBtn = document.getElementById("modalDismissBtn");
  const appModalBackdrop = document.getElementById("appModalBackdrop");

  let currentLang = "ar";
  try {
    currentLang = localStorage.getItem("book_splitter_lang") || "ar";
  } catch (_) {}

  function t(key, ...args) {
    const dict = i18n[currentLang] || i18n.ar || i18n.en || {};
    const val = dict[key] !== undefined ? dict[key] : ((i18n.ar && i18n.ar[key]) || (i18n.en && i18n.en[key]) || key);
    return typeof val === "function" ? val(...args) : val;
  }

  function setLanguage(lang) {
    if (lang !== "en" && lang !== "ar") lang = "ar";
    currentLang = lang;
    try {
      localStorage.setItem("book_splitter_lang", lang);
    } catch (_) {}

    document.documentElement.lang = lang;
    document.documentElement.dir = lang === "ar" ? "rtl" : "ltr";
    document.title = lang === "ar"
      ? "مُقسِّم الكتب — أداة تقسيم ملفات PDF"
      : "PDF Splitter — PDF Chapter & Range Splitter";

    langEnBtn.classList.toggle("active", lang === "en");
    langArBtn.classList.toggle("active", lang === "ar");

    document.querySelectorAll("[data-i18n]").forEach((el) => {
      el.textContent = t(el.getAttribute("data-i18n"));
    });
    document.querySelectorAll("[data-i18n-placeholder]").forEach((el) => {
      el.placeholder = t(el.getAttribute("data-i18n-placeholder"));
    });
    document.querySelectorAll("[data-i18n-title]").forEach((el) => {
      el.title = t(el.getAttribute("data-i18n-title"));
    });

    if (currentBook) {
      bookPagesBadge.textContent = t("bookPagesBadge", currentBook.total_pages);
    }
    renderSplitPoints();
    updatePartsCalculation();

    if (lastSplitData) {
      statusText.textContent = t("statusSuccess", lastSplitData.parts.length);
      const retMinutes = lastSplitData.retention_minutes || 15;
      retentionText.textContent = t("retentionNotice", retMinutes);
      if (lastSplitData.zip_download_url) {
        const mb = lastSplitData.zip_size ? (lastSplitData.zip_size / (1024 * 1024)).toFixed(1) : "0";
        downloadZipBtn.textContent = t("btnDownloadZip", mb);
      }
      renderResultsTable(lastSplitData.parts);
    }
  }

  langEnBtn.addEventListener("click", () => setLanguage("en"));
  langArBtn.addEventListener("click", () => setLanguage("ar"));

  function openModal() {
    appModalBackdrop.style.display = "flex";
    document.body.style.overflow = "hidden";
  }

  function closeModal() {
    appModalBackdrop.style.display = "none";
    document.body.style.overflow = "";
  }

  openAppModalBtn.addEventListener("click", openModal);
  closeAppModalBtn.addEventListener("click", closeModal);
  modalDismissBtn.addEventListener("click", closeModal);
  appModalBackdrop.addEventListener("click", (e) => {
    if (e.target === appModalBackdrop) closeModal();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && appModalBackdrop.style.display === "flex") closeModal();
  });

  async function loadServerConfig() {
    try {
      const res = await fetch("/api/config");
      if (res.ok) {
        cachedServerConfig = await res.json();
        isStandaloneMode = !!cachedServerConfig.standalone;
        if (isStandaloneMode) {
          connectStandaloneEvents();
        }
        return cachedServerConfig;
      }
    } catch (e) {
      console.warn("Failed to load server config:", e);
    }
    return null;
  }

  // In standalone mode, hold an SSE connection so background tab throttling and F5 reloads
  // do not kill the local server prematurely.
  function connectStandaloneEvents() {
    if (!isStandaloneMode || sseSource) return;
    sseSource = new EventSource("/api/events");
    sseSource.onerror = () => {
      // EventSource automatically reconnects if the server is still up.
    };
  }

  function purgeSessionOnServer(sessionId) {
    if (!sessionId) return;
    const payload = JSON.stringify({ session_id: sessionId });
    fetch("/api/clear", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "X-CSRF-Token": csrfToken
      },
      body: payload,
      keepalive: true
    }).catch(() => {
      if (navigator.sendBeacon) {
        const blob = new Blob([payload], { type: "application/json" });
        navigator.sendBeacon(`/api/clear?csrf_token=${encodeURIComponent(csrfToken)}`, blob);
      }
    });
  }

  // On VPS mode, purge active session on pagehide; in standalone mode, SSE disconnect handles shutdown.
  window.addEventListener("pagehide", () => {
    if (!isStandaloneMode && currentSessionId) {
      purgeSessionOnServer(currentSessionId);
    }
  });

  clearBookBtn.addEventListener("click", () => {
    if (!currentBook) return;
    if (!confirm(t("confirmClearBook"))) return;

    if (currentXhr) {
      currentXhr.abort();
      currentXhr = null;
    }
    if (currentSessionId) {
      purgeSessionOnServer(currentSessionId);
      currentSessionId = "";
    }
    currentBook = null;
    currentFile = null;
    lastSplitData = null;
    splitPoints = [];
    fileInput.value = "";

    uploadProgress.style.display = "none";
    uploadProgress.classList.remove("active");
    bookInfo.classList.remove("active");
    splitSection.classList.remove("active");
    resultsCard.style.display = "none";

    renderSplitPoints();
    updatePartsCalculation();
  });

  function showUploadProgress(fileName, fileSize) {
    progressFileName.textContent = fileName;
    progressStatusText.textContent = t("uploadingPdf");
    progressPercentBadge.textContent = "0%";
    progressBarFill.style.width = "0%";
    const mbTotal = (fileSize / (1024 * 1024)).toFixed(1);
    progressBytesText.textContent = `0.0 MB / ${mbTotal} MB`;
    progressStorageText.textContent = "";
    uploadProgress.style.display = "block";
    uploadProgress.classList.add("active");
  }

  function updateProgressMetrics(loaded, total, percent, statusStr, storageStr) {
    const pct = Math.min(100, Math.max(0, percent));
    progressBarFill.style.width = `${pct}%`;
    progressPercentBadge.textContent = `${pct}%`;
    const mbLoaded = (loaded / (1024 * 1024)).toFixed(1);
    const mbTotal = (total / (1024 * 1024)).toFixed(1);
    progressBytesText.textContent = `${mbLoaded} MB / ${mbTotal} MB`;
    if (statusStr) progressStatusText.textContent = statusStr;
    if (storageStr) progressStorageText.textContent = storageStr;
  }

  function setUploadStatus(statusStr, percent, storageStr) {
    if (statusStr) progressStatusText.textContent = statusStr;
    if (percent !== undefined) {
      progressBarFill.style.width = `${percent}%`;
      progressPercentBadge.textContent = `${percent}%`;
    }
    if (storageStr) progressStorageText.textContent = storageStr;
  }

  function handleUploadError(errMsg) {
    uploadProgress.style.display = "none";
    uploadProgress.classList.remove("active");
    alert(t("uploadFailed") + errMsg);
  }

  function onInspectionSuccess(data) {
    currentBook = data;
    currentSessionId = data.session_id || "";

    uploadProgress.style.display = "none";
    uploadProgress.classList.remove("active");

    bookName.textContent = data.file_name;
    bookSize.textContent = `${(data.file_size / (1024 * 1024)).toFixed(2)} MB`;
    bookPagesBadge.textContent = t("bookPagesBadge", data.total_pages);

    bookInfo.classList.add("active");
    splitSection.classList.add("active");
    resultsCard.style.display = "none";

    splitPoints = splitPoints.filter((p) => p < currentBook.total_pages);
    renderSplitPoints();
    updatePartsCalculation();
  }

  async function getConfig() {
    if (cachedServerConfig) return cachedServerConfig;
    return await loadServerConfig();
  }

  async function handleFileSelect(file) {
    if (!file.name.toLowerCase().endsWith(".pdf")) {
      alert(t("invalidPdf"));
      return;
    }

    const cfg = await getConfig();
    if (cfg && cfg.max_upload_mb && file.size > cfg.max_upload_mb * 1024 * 1024) {
      alert(t("fileTooLarge", cfg.max_upload_mb));
      return;
    }

    if (currentXhr) {
      currentXhr.abort();
      currentXhr = null;
    }

    if (currentSessionId) {
      purgeSessionOnServer(currentSessionId);
      currentSessionId = "";
    }

    currentFile = file;
    bookInfo.classList.remove("active");
    splitSection.classList.remove("active");
    resultsCard.style.display = "none";

    showUploadProgress(file.name, file.size);

    if (cfg && cfg.s3_enabled) {
      uploadDirectToS3(file);
    } else {
      uploadDirectToLocal(file);
    }
  }

  async function uploadDirectToS3(file) {
    setUploadStatus(t("requestingPresign"), 5, t("storageS3"));

    try {
      const presignRes = await fetch("/api/s3/presign", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-CSRF-Token": csrfToken
        },
        body: JSON.stringify({
          filename: file.name,
          file_size: file.size
        })
      });

      if (!presignRes.ok) {
        return uploadDirectToLocal(file);
      }

      const presignData = await presignRes.json();
      const uploadUrl = presignData.upload_url || presignData.url;
      const s3Key = presignData.key;
      const sessionId = presignData.session_id || "";
      const signedCT = presignData.content_type || "application/pdf";

      if (!uploadUrl || !s3Key) {
        return uploadDirectToLocal(file);
      }

      setUploadStatus(t("uploadingDirectS3"), 10, t("storageS3"));

      const xhr = new XMLHttpRequest();
      currentXhr = xhr;

      xhr.upload.onprogress = (e) => {
        if (e.lengthComputable) {
          const pct = Math.round((e.loaded / e.total) * 100);
          updateProgressMetrics(e.loaded, e.total, pct, t("uploadingDirectS3"), t("storageS3"));
        }
      };

      xhr.onload = async () => {
        if (currentXhr === xhr) currentXhr = null;
        if (xhr.status >= 200 && xhr.status < 300) {
          setUploadStatus(t("inspectingPdf"), 100, t("storageS3"));
          try {
            const inspectRes = await fetch("/api/inspect", {
              method: "POST",
              headers: {
                "Content-Type": "application/json",
                "X-CSRF-Token": csrfToken
              },
              body: JSON.stringify({
                session_id: sessionId,
                s3_key: s3Key,
                filename: file.name
              })
            });
            const inspectData = await inspectRes.json();
            if (!inspectRes.ok || inspectData.error) {
              handleUploadError(inspectData.error || `HTTP ${inspectRes.status}`);
              return;
            }
            onInspectionSuccess(inspectData);
          } catch (err) {
            handleUploadError(err.message);
          }
        } else {
          uploadDirectToLocal(file);
        }
      };

      xhr.onerror = () => {
        if (currentXhr === xhr) currentXhr = null;
        uploadDirectToLocal(file);
      };

      xhr.open("PUT", uploadUrl, true);
      xhr.setRequestHeader("Content-Type", signedCT);
      xhr.send(file);
    } catch (_) {
      uploadDirectToLocal(file);
    }
  }

  function uploadDirectToLocal(file) {
    setUploadStatus(t("uploadingPdf"), 5, t("storageLocal"));

    const xhr = new XMLHttpRequest();
    currentXhr = xhr;

    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) {
        const pct = Math.round((e.loaded / e.total) * 100);
        updateProgressMetrics(e.loaded, e.total, pct, t("uploadingPdf"), t("storageLocal"));
      }
    };

    xhr.onload = () => {
      if (currentXhr === xhr) currentXhr = null;
      if (xhr.status >= 200 && xhr.status < 300) {
        try {
          const inspectData = JSON.parse(xhr.responseText);
          if (inspectData.error) {
            handleUploadError(inspectData.error);
            return;
          }
          setUploadStatus(t("uploadComplete"), 100, t("storageLocal"));
          setTimeout(() => onInspectionSuccess(inspectData), 200);
        } catch (e) {
          handleUploadError("Invalid server response: " + e.message);
        }
      } else {
        let errMsg = `Server returned status ${xhr.status}`;
        try {
          const data = JSON.parse(xhr.responseText);
          if (data.error) errMsg = data.error;
        } catch (_) {}
        handleUploadError(errMsg);
      }
    };

    xhr.onerror = () => {
      if (currentXhr === xhr) currentXhr = null;
      handleUploadError(t("networkError"));
    };

    const formData = new FormData();
    formData.append("file", file);
    xhr.open("POST", "/api/inspect", true);
    xhr.setRequestHeader("X-CSRF-Token", csrfToken);
    xhr.send(formData);
  }

  dropzone.addEventListener("click", () => fileInput.click());
  dropzone.addEventListener("dragover", (e) => {
    e.preventDefault();
    dropzone.classList.add("dragover");
  });
  dropzone.addEventListener("dragleave", () => dropzone.classList.remove("dragover"));
  dropzone.addEventListener("drop", (e) => {
    e.preventDefault();
    dropzone.classList.remove("dragover");
    if (e.dataTransfer.files.length > 0) {
      handleFileSelect(e.dataTransfer.files[0]);
    }
  });
  fileInput.addEventListener("change", (e) => {
    if (e.target.files.length > 0) {
      handleFileSelect(e.target.files[0]);
    }
  });

  function addPointsFromInput(inputStr) {
    if (!inputStr || !inputStr.trim()) return;
    const tokens = inputStr.split(/[,،\s]+/).filter((tok) => tok.trim().length > 0);
    const warnings = [];

    for (const token of tokens) {
      const num = parseInt(token, 10);
      if (isNaN(num) || num <= 0) {
        warnings.push(t("errorNotValidPositive", token));
        continue;
      }
      if (currentBook && num >= currentBook.total_pages) {
        warnings.push(t("errorPageMustBeLess", num, currentBook.total_pages));
        continue;
      }
      if (!splitPoints.includes(num)) {
        splitPoints.push(num);
      }
    }

    splitPoints.sort((a, b) => a - b);
    renderSplitPoints();
    updatePartsCalculation();

    if (warnings.length > 0) {
      alert(warnings.join("\n"));
    }

    splitInput.value = "";
    splitInput.focus();
  }

  addPointBtn.addEventListener("click", () => addPointsFromInput(splitInput.value));
  splitInput.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      addPointsFromInput(splitInput.value);
    }
  });

  presetExampleBtn.addEventListener("click", () => {
    const example = [10, 20, 40];
    splitPoints = example.filter((p) => !currentBook || p < currentBook.total_pages);
    renderSplitPoints();
    updatePartsCalculation();
  });

  clearPointsBtn.addEventListener("click", () => {
    splitPoints = [];
    renderSplitPoints();
    updatePartsCalculation();
  });

  function removePoint(p) {
    splitPoints = splitPoints.filter((x) => x !== p);
    renderSplitPoints();
    updatePartsCalculation();
  }

  function renderSplitPoints() {
    chipsContainer.replaceChildren();
    if (splitPoints.length === 0) {
      const span = document.createElement("span");
      span.className = "muted-note";
      span.textContent = t("noPointsYet");
      chipsContainer.appendChild(span);
      return;
    }

    splitPoints.forEach((p) => {
      const chip = document.createElement("div");
      chip.className = "chip";

      const label = document.createElement("span");
      label.textContent = t("chipPage", p);

      const rm = document.createElement("button");
      rm.type = "button";
      rm.className = "chip-remove";
      rm.title = t("chipRemoveTitle");
      rm.textContent = "\u00d7";
      rm.addEventListener("click", () => removePoint(p));

      chip.append(label, rm);
      chipsContainer.appendChild(chip);
    });
  }

  function createPartCard(badgeText, rangeText, countText, countBadgeText, isRest) {
    const card = document.createElement("div");
    card.className = isRest ? "part-card is-rest" : "part-card";

    const titleWrap = document.createElement("div");
    titleWrap.className = "part-title";

    const badge = document.createElement("span");
    badge.className = isRest ? "badge badge-rest" : "badge badge-accent";
    badge.textContent = badgeText;

    const rangeSpan = document.createElement("span");
    rangeSpan.textContent = rangeText;
    titleWrap.append(badge, rangeSpan);

    const metaWrap = document.createElement("div");
    metaWrap.className = "part-meta";

    const countSpan = document.createElement("span");
    countSpan.className = isRest ? "part-range part-range-rest" : "part-range";
    countSpan.textContent = countText;

    const countBadge = document.createElement("span");
    countBadge.className = isRest ? "part-count-badge rest" : "part-count-badge";
    countBadge.textContent = countBadgeText;

    metaWrap.append(countSpan, countBadge);
    card.append(titleWrap, metaWrap);
    return card;
  }

  function updatePartsCalculation() {
    if (!currentBook) return;
    const total = currentBook.total_pages;
    partsList.replaceChildren();

    let start = 1;
    let partIdx = 1;

    splitPoints.forEach((pt) => {
      if (pt >= start) {
        const count = pt - start + 1;
        partsList.appendChild(
          createPartCard(
            t("partBadge", partIdx),
            t("partRange", start, pt),
            t("partPagesCount", count),
            t("partPagesBadge", count),
            false
          )
        );
        start = pt + 1;
        partIdx++;
      }
    });

    if (start <= total) {
      const restCount = total - start + 1;
      partsList.appendChild(
        createPartCard(
          t("restPartBadge", partIdx),
          t("restPartRange", start, total),
          t("restRemainingPages", restCount),
          t("partPagesBadge", restCount),
          true
        )
      );
    }
  }

  splitNowBtn.addEventListener("click", async () => {
    if (!currentBook || (!currentFile && !currentSessionId)) {
      alert(t("errorSelectBookFirst"));
      return;
    }

    splitNowBtn.disabled = true;
    splitSpinner.style.display = "inline-block";
    splitNowBtn.textContent = t("btnSplitting");

    const formData = new FormData();
    if (currentSessionId) {
      formData.append("session_id", currentSessionId);
    } else if (currentFile) {
      formData.append("file", currentFile);
    }
    formData.append("split_points", splitPoints.join(","));

    try {
      const res = await fetch("/api/split", {
        method: "POST",
        headers: { "X-CSRF-Token": csrfToken },
        body: formData
      });
      const data = await res.json();

      splitNowBtn.disabled = false;
      splitSpinner.style.display = "none";
      splitNowBtn.textContent = t("btnSplit");

      if (!res.ok || data.error) {
        resultsCard.style.display = "block";
        statusBox.className = "status-box error active";
        statusText.textContent = t("splitFailed") + (data.error || `HTTP ${res.status}`);
        downloadZipBtn.style.display = "none";
        openFolderBtn.style.display = "none";
        return;
      }

      if (data.session_id) currentSessionId = data.session_id;
      lastSplitData = data;

      resultsCard.style.display = "block";
      statusBox.className = "status-box success active";
      statusText.textContent = t("statusSuccess", data.parts.length);

      const retMinutes = data.retention_minutes || 15;
      retentionText.textContent = t("retentionNotice", retMinutes);

      if (data.zip_download_url) {
        downloadZipBtn.href = data.zip_download_url;
        const mb = data.zip_size ? (data.zip_size / (1024 * 1024)).toFixed(1) : "0";
        downloadZipBtn.textContent = t("btnDownloadZip", mb);
        downloadZipBtn.style.display = "inline-flex";
      } else {
        downloadZipBtn.style.display = "none";
      }

      openFolderBtn.style.display = data.can_open_folder ? "inline-flex" : "none";
      renderResultsTable(data.parts);
    } catch (err) {
      splitNowBtn.disabled = false;
      splitSpinner.style.display = "none";
      splitNowBtn.textContent = t("btnSplit");
      alert(t("splitFailed") + err.message);
    }
  });

  function renderResultsTable(parts) {
    resultsTableBody.replaceChildren();
    if (!parts) return;

    parts.forEach((p) => {
      const row = document.createElement("tr");

      const tdPart = document.createElement("td");
      const strong = document.createElement("strong");
      strong.textContent = t("partBadge", p.part_number) + (p.is_rest ? t("tablePartRest") : "");
      tdPart.appendChild(strong);

      const tdName = document.createElement("td");
      const code = document.createElement("code");
      code.className = "output-filename";
      code.textContent = p.file_name;
      tdName.appendChild(code);

      const tdRange = document.createElement("td");
      tdRange.textContent = `${p.start_page} \u2013 ${p.end_page}`;

      const tdPages = document.createElement("td");
      const badge = document.createElement("span");
      badge.className = "badge";
      badge.textContent = t("tablePagesCount", p.page_count);
      tdPages.appendChild(badge);

      const tdSize = document.createElement("td");
      tdSize.textContent = p.file_size ? `${(p.file_size / 1024).toFixed(0)} KB` : "-";

      const tdAction = document.createElement("td");
      const link = document.createElement("a");
      link.href = p.download_url;
      link.className = "btn btn-secondary btn-sm";
      link.setAttribute("download", p.file_name || "");
      link.textContent = t("btnDownloadPart");
      tdAction.appendChild(link);

      row.append(tdPart, tdName, tdRange, tdPages, tdSize, tdAction);
      resultsTableBody.appendChild(row);
    });
  }

  openFolderBtn.addEventListener("click", async () => {
    if (!currentSessionId) return;
    try {
      await fetch("/api/open-folder", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-CSRF-Token": csrfToken
        },
        body: JSON.stringify({ session_id: currentSessionId })
      });
    } catch (err) {
      console.error("Failed to open output folder:", err);
    }
  });

  setLanguage(currentLang);
  loadServerConfig();
})();
