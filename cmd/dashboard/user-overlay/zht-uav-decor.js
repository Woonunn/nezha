(() => {
    const FAVICON_HREF = "/favicon.ico?v=2"

    function mountBackdrop() {
        if (document.getElementById("zht-uav-stage")) return

        const stage = document.createElement("div")
        stage.id = "zht-uav-stage"
        stage.setAttribute("aria-hidden", "true")
        stage.innerHTML = `
            <div class="zht-uav-atmosphere"></div>
            <div class="zht-uav-grid"></div>
            <div class="zht-cad-layer">
                <figure class="zht-cad-item zht-cad-keel-max">
                    <div class="zht-cad-float">
                        <img src="/aircraft/keel-max-cad.webp" alt="" loading="eager" />
                    </div>
                </figure>
            </div>
        `
        document.body.prepend(stage)
    }

    function ensurePerchedDrone() {
        const overview = document.querySelector(".server-overview")
        if (!overview) return

        if (overview.getAttribute("aria-busy") === "true") return
        const networkCard = overview.lastElementChild
        if (!networkCard) return
        if (networkCard.querySelector(".zht-perched-uav")) return

        const wrapper = document.createElement("div")
        wrapper.className = "zht-perched-uav"
        wrapper.setAttribute("aria-hidden", "true")
        wrapper.innerHTML = `
            <img class="zht-perched-uav-img" src="/aircraft/naga-x4-cad.webp" alt="" loading="eager" />
            <span class="zht-perched-uav-pad"></span>
        `
        networkCard.appendChild(wrapper)
    }

    function hideThemeCredit() {
        const themeLink = document.querySelector('footer a[href*="nezha-dash"]')
        const creditSection = themeLink && themeLink.closest("section")
        if (creditSection && creditSection.style.display !== "none") {
            creditSection.style.display = "none"
        }
    }

    function syncFavicon() {
        const links = document.querySelectorAll('link[rel*="icon"]')
        links.forEach((link) => {
            if (link.getAttribute("rel") === "apple-touch-icon") return
            if (link.getAttribute("href") !== FAVICON_HREF) {
                link.setAttribute("href", FAVICON_HREF)
                link.removeAttribute("type")
            }
        })
    }

    function init() {
        mountBackdrop()
        ensurePerchedDrone()
        hideThemeCredit()
        syncFavicon()

        const observer = new MutationObserver(() => {
            ensurePerchedDrone()
            hideThemeCredit()
            syncFavicon()
        })
        observer.observe(document.body, { childList: true, subtree: true })
        observer.observe(document.head, { childList: true, subtree: true, attributes: true })
    }

    if (document.readyState === "loading") {
        document.addEventListener("DOMContentLoaded", init, { once: true })
    } else {
        init()
    }
})()
